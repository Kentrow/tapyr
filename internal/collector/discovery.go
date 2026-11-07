package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/state"
)

// inventoryTTLFactor and staleAfterFactor set the default inventory TTL and
// staleAfter: 3x the effective interval.
const (
	inventoryTTLFactor = 3
	staleAfterFactor   = 3
)

// discovery keeps the instances of fan-out jobs in line with the inventory
// of their parent instances (see docs/spec/05-collection.md, "Discovery"):
// children are created and removed after every successful parent run, start
// from the cached inventory after a restart, and are removed when the
// inventory of their parent expires.
type discovery struct {
	sched *scheduler.Scheduler
	store state.Store
	// retire withdraws the series of a removed instance; called with d.mu
	// held.
	retire func(instance string)
	logger *slog.Logger
	// changed is signaled (without blocking) when instances come or go.
	changed chan struct{}
	// observe, when set, is called with the number of scheduled instances
	// whenever it changes (tests).
	observe func(n int)

	mu sync.Mutex
	// static are the configured instances, models the fan-out jobs by the
	// name of their parent job.
	static []*runtime.Job
	models map[string][]*runtime.Job
	// active are the scheduled instances by key, children the child keys of
	// a parent instance, refreshed the time of its inventory.
	active    map[string]*runtime.Job
	children  map[string][]string
	refreshed map[string]time.Time
}

func newDiscovery(jobs []*runtime.Job, store state.Store, retire func(string), logger *slog.Logger) *discovery {
	d := &discovery{
		store: store, retire: retire, logger: logger, changed: make(chan struct{}, 1),
		models: map[string][]*runtime.Job{}, active: map[string]*runtime.Job{},
		children: map[string][]string{}, refreshed: map[string]time.Time{},
	}
	for _, j := range jobs {
		if j.ForEach != nil {
			d.models[j.ForEach.ParentJob] = append(d.models[j.ForEach.ParentJob], j)
			continue
		}
		d.static = append(d.static, j)
		d.active[j.Instance] = j
	}
	return d
}

// attach sets the scheduler of the instances and starts the children of the
// configured parents from the cached inventory.
func (d *discovery) attach(ctx context.Context, sched *scheduler.Scheduler, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sched = sched
	for _, j := range d.static {
		sched.Add(j)
	}
	for _, j := range d.static {
		d.loadCachedLocked(ctx, j, now)
	}
	d.notifyLocked()
}

// deliver calls fn while the instance is scheduled, so that samples of a
// run finishing after its instance was removed never reach the outputs
// once the instance series are retired.
func (d *discovery) deliver(instance string, fn func()) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[instance]; !ok {
		return false
	}
	fn()
	return true
}

// do calls fn with the instances locked: active maps the scheduled
// instances by key.
func (d *discovery) do(fn func(active map[string]*runtime.Job)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.active)
}

// update stores the inventory of a successful parent run and reconciles the
// children of the parent instance.
func (d *discovery) update(ctx context.Context, parent *runtime.Job, items []mapping.InventoryItem, now time.Time) error {
	cached := make([]state.InventoryItem, 0, len(items))
	for _, it := range items {
		fields, err := json.Marshal(it.Fields)
		if err != nil {
			return fmt.Errorf("encoding inventory item %q: %w", it.Key, err)
		}
		cached = append(cached, state.InventoryItem{Key: it.Key, Fields: fields})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[parent.Instance]; !ok {
		return nil
	}
	if err := d.store.SaveInventory(ctx, parent.Instance, cached, now); err != nil {
		return err
	}
	d.reconcileLocked(ctx, parent, items, now, now)
	return nil
}

// expire removes the children of the parent instances whose inventory was
// not refreshed within its TTL.
func (d *discovery) expire(ctx context.Context, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(d.refreshed)) {
		at, ok := d.refreshed[key]
		parent := d.active[key]
		if !ok || parent == nil || now.Sub(at) <= d.ttl(parent) {
			continue
		}
		d.logger.Warn("inventory expired", "component", "discovery", "provider", parent.Provider, "account", parent.Account,
			"job", parent.Name, "instance", key, "refreshed", at, "children", len(d.children[key]))
		for _, child := range d.children[key] {
			d.removeLocked(ctx, child)
		}
		delete(d.children, key)
		delete(d.refreshed, key)
		d.signal()
		d.notifyLocked()
	}
}

// planEntries returns the planner inputs: the configured instances, and the
// fan-out models with at least one instance.
func (d *discovery) planEntries() []PlanEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	counts := map[string]int{}
	for _, j := range d.active {
		counts[scheduler.Group(j)]++
	}
	entries := make([]PlanEntry, 0, len(d.static))
	for _, j := range d.static {
		entries = append(entries, PlanEntry{Job: j, Fanout: 1})
	}
	for _, parent := range slices.Sorted(maps.Keys(d.models)) {
		for _, m := range d.models[parent] {
			if n := counts[scheduler.Group(m)]; n > 0 {
				entries = append(entries, PlanEntry{Job: m, Fanout: n})
			}
		}
	}
	return entries
}

// activeKeys returns the keys of the scheduled instances.
func (d *discovery) activeKeys() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]bool, len(d.active))
	for key := range d.active {
		out[key] = true
	}
	return out
}

// counts returns the number of active instances and of fan-out instances.
func (d *discovery) counts() (active, dynamic int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active), len(d.active) - len(d.static)
}

// ttl is the inventory TTL of a parent instance.
func (d *discovery) ttl(j *runtime.Job) time.Duration {
	if j.InventoryTTL > 0 {
		return j.InventoryTTL
	}
	return inventoryTTLFactor * d.interval(j)
}

// staleAfter is the delay after which the series of an instance without a
// successful run are withdrawn.
func (d *discovery) staleAfter(j *runtime.Job) time.Duration {
	if j.StaleAfter > 0 {
		return j.StaleAfter
	}
	return staleAfterFactor * d.interval(j)
}

// interval is the effective interval of an instance.
func (d *discovery) interval(j *runtime.Job) time.Duration {
	if d.sched != nil {
		if eff := d.sched.Interval(scheduler.Group(j)); eff > 0 {
			return eff
		}
	}
	return j.Interval
}

func (d *discovery) isParent(j *runtime.Job) bool {
	return j.Role != config.RoleMetrics && len(d.models[j.Name]) > 0
}

// loadCachedLocked starts the children of a parent instance from its cached
// inventory, when fresh.
func (d *discovery) loadCachedLocked(ctx context.Context, parent *runtime.Job, now time.Time) {
	if !d.isParent(parent) {
		return
	}
	log := d.logger.With("component", "discovery", "provider", parent.Provider, "account", parent.Account,
		"job", parent.Name, "instance", parent.Instance)
	cached, at, ok, err := d.store.LoadInventory(ctx, parent.Instance)
	if err != nil {
		log.Warn("loading the cached inventory", "error", err)
		return
	}
	if !ok || now.Sub(at) > d.ttl(parent) {
		return
	}
	items := make([]mapping.InventoryItem, 0, len(cached))
	for _, c := range cached {
		v, err := mapping.DecodeJSON(c.Fields)
		fields, isMap := v.(map[string]any)
		if err != nil || !isMap {
			log.Warn("ignoring a cached inventory item", "key", c.Key, "error", err)
			continue
		}
		items = append(items, mapping.InventoryItem{Key: c.Key, Fields: fields})
	}
	d.reconcileLocked(ctx, parent, items, at, now)
	log.Info("children started from the cached inventory", "items", len(items), "refreshed", at)
}

// reconcileLocked creates and removes the children of a parent instance to
// match its inventory items.
func (d *discovery) reconcileLocked(ctx context.Context, parent *runtime.Job, items []mapping.InventoryItem, at, now time.Time) {
	d.refreshed[parent.Instance] = at
	want := map[string]*runtime.Job{}
	var order []string
	for _, m := range d.models[parent.Name] {
		if m.Target != parent.Target {
			continue
		}
		n := 0
		for _, it := range items {
			child, ok, err := m.Child(parent, it, now)
			if err != nil {
				d.logger.Warn("inventory item skipped", "component", "discovery", "provider", m.Provider, "account", m.Account,
					"job", m.Name, "item", it.Key, "error", err)
				continue
			}
			if !ok {
				continue
			}
			if _, dup := want[child.Instance]; dup {
				continue
			}
			if m.ForEach.MaxInstances > 0 && n >= m.ForEach.MaxInstances {
				d.logger.Warn("forEach.maxInstances reached: instances dropped", "component", "discovery", "provider", m.Provider,
					"account", m.Account, "job", m.Name, "max_instances", m.ForEach.MaxInstances)
				break
			}
			n++
			want[child.Instance] = child
			order = append(order, child.Instance)
		}
	}
	changed := false
	for _, key := range d.children[parent.Instance] {
		if _, ok := want[key]; !ok {
			d.removeLocked(ctx, key)
			changed = true
		}
	}
	d.children[parent.Instance] = order
	for _, key := range order {
		if _, ok := d.active[key]; ok {
			continue
		}
		child := want[key]
		d.active[key] = child
		d.sched.Add(child)
		changed = true
		d.loadCachedLocked(ctx, child, now)
	}
	if changed {
		d.signal()
		d.notifyLocked()
	}
}

// removeLocked unschedules an instance and its descendants, retires their
// series and drops their cached inventory.
func (d *discovery) removeLocked(ctx context.Context, key string) {
	if _, ok := d.active[key]; !ok {
		return
	}
	for _, child := range d.children[key] {
		d.removeLocked(ctx, child)
	}
	delete(d.children, key)
	delete(d.refreshed, key)
	delete(d.active, key)
	d.sched.Remove(key)
	d.retire(key)
	if err := d.store.DeleteInventory(ctx, key); err != nil {
		d.logger.Warn("deleting the cached inventory", "component", "discovery", "instance", key, "error", err)
	}
}

func (d *discovery) notifyLocked() {
	if d.observe != nil {
		d.observe(len(d.active))
	}
}

func (d *discovery) signal() {
	select {
	case d.changed <- struct{}{}:
	default:
	}
}
