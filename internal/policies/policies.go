package policies

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/ubuntu/adsys/internal/consts"
	"github.com/ubuntu/adsys/internal/decorate"
	log "github.com/ubuntu/adsys/internal/grpc/logstreamer"
	"github.com/ubuntu/adsys/internal/i18n"
	"github.com/ubuntu/adsys/internal/policies/dconf"
	"github.com/ubuntu/adsys/internal/policies/entry"
	"github.com/ubuntu/adsys/internal/policies/gdm"
	"github.com/ubuntu/adsys/internal/policies/privilege"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

// Manager handles all managers for various policy handlers.
type Manager struct {
	policiesCacheDir string

	dconf     *dconf.Manager
	privilege *privilege.Manager
	gdm       *gdm.Manager

	subcriptionDbus dbus.BusObject

	sync.RWMutex
	subscriptionEnabled bool
}

type options struct {
	cacheDir     string
	dconfDir     string
	sudoersDir   string
	policyKitDir string
	gdm          *gdm.Manager
}

// Option reprents an optional function to change Policies behavior.
type Option func(*options) error

// WithCacheDir specifies a personalized daemon cache directory.
func WithCacheDir(p string) Option {
	return func(o *options) error {
		o.cacheDir = p
		return nil
	}
}

// WithDconfDir specifies a personalized dconf directory.
func WithDconfDir(p string) Option {
	return func(o *options) error {
		o.dconfDir = p
		return nil
	}
}

// WithSudoersDir specifies a personalized sudoers directory.
func WithSudoersDir(p string) Option {
	return func(o *options) error {
		o.sudoersDir = p
		return nil
	}
}

// WithPolicyKitDir specifies a personalized policykit directory.
func WithPolicyKitDir(p string) Option {
	return func(o *options) error {
		o.policyKitDir = p
		return nil
	}
}

// NewManager returns a new manager with all default policy handlers.
func NewManager(bus *dbus.Conn, opts ...Option) (m *Manager, err error) {
	defer decorate.OnError(&err, i18n.G("can't create a new policy handlers manager"))

	// defaults
	args := options{
		cacheDir: consts.DefaultCacheDir,
		gdm:      nil,
	}
	// applied options (including dconf manager used by gdm)
	for _, o := range opts {
		if err := o(&args); err != nil {
			return nil, err
		}
	}
	// dconf manager
	dconfManager := &dconf.Manager{}
	if args.dconfDir != "" {
		dconfManager = dconf.NewWithDconfDir(args.dconfDir)
	}

	// privilege manager
	privilegeManager := privilege.NewWithDirs(args.sudoersDir, args.policyKitDir)

	// inject applied dconf mangager if we need to build a gdm manager
	if args.gdm == nil {
		if args.gdm, err = gdm.New(gdm.WithDconf(dconfManager)); err != nil {
			return nil, err
		}
	}

	policiesCacheDir := filepath.Join(args.cacheDir, PoliciesCacheBaseName)
	if err := os.MkdirAll(policiesCacheDir, 0700); err != nil {
		return nil, err
	}

	subscriptionDbus := bus.Object(consts.SubcriptionDbusRegisteredName,
		dbus.ObjectPath(consts.SubcriptionDbusObjectPath))

	return &Manager{
		policiesCacheDir: policiesCacheDir,

		dconf:     dconfManager,
		privilege: privilegeManager,
		gdm:       args.gdm,

		subcriptionDbus: subscriptionDbus,
	}, nil
}

// Policies is the list of GPOs applied to a particular object, with the global data cache.
type Policies struct {
	GPOs []GPO
	Data io.ReaderAt `yaml:"-"`
}

// NewFromCache returns cached policies loaded from the p json file.
func NewFromCache(p string) (pols Policies, err error) {
	defer decorate.OnError(&err, i18n.G("can't get cached policies from %s"), p)

	d, err := os.ReadFile(p)
	if err != nil {
		return pols, err
	}

	if err := yaml.Unmarshal(d, &pols); err != nil {
		return pols, err
	}
	return pols, nil
}

// Save serializes in p the policies.
func (pols *Policies) Save(p string) (err error) {
	defer decorate.OnError(&err, i18n.G("can't save policies to %s"), p)

	d, err := yaml.Marshal(pols)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, d, 0600); err != nil {
		return err
	}

	return nil
}

// GetUniqueRules return order rules, with one entry per key for a given type.
// Returned file is a map of type to its entries.
func (pols Policies) GetUniqueRules() map[string][]entry.Entry {
	r := make(map[string][]entry.Entry)
	keys := make(map[string][]string)

	// Dedup entries, first GPO wins for a given type + key
	dedup := make(map[string]map[string]entry.Entry)
	seen := make(map[string]struct{})
	for _, gpo := range pols.GPOs {
		for t, entries := range gpo.Rules {
			if dedup[t] == nil {
				dedup[t] = make(map[string]entry.Entry)
			}
			for _, e := range entries {
				switch e.Strategy {
				case entry.StrategyAppend:
					// We skip disabled keys as we only append enabled one.
					if e.Disabled {
						continue
					}
					var keyAlreadySeen bool
					// If there is an existing value, prepend new value to it. We are analyzing GPOs in reverse order (closest first).
					if _, exists := seen[t+e.Key]; exists {
						keyAlreadySeen = true
						// We have seen a closest key which is an override. We don’t append furthest append values.
						if dedup[t][e.Key].Strategy != entry.StrategyAppend {
							continue
						}
						e.Value = e.Value + "\n" + dedup[t][e.Key].Value
						// Keep closest meta value.
						e.Meta = dedup[t][e.Key].Meta
					}
					dedup[t][e.Key] = e
					if keyAlreadySeen {
						continue
					}

				default:
					// override case
					if _, exists := seen[t+e.Key]; exists {
						continue
					}
					dedup[t][e.Key] = e
				}

				keys[t] = append(keys[t], e.Key)
				seen[t+e.Key] = struct{}{}
			}
		}
	}

	// For each t, order entries by ascii order
	for t := range dedup {
		var entries []entry.Entry
		sort.Strings(keys[t])
		for _, k := range keys[t] {
			entries = append(entries, dedup[t][k])
		}
		r[t] = entries
	}

	return r
}

// ApplyPolicies generates a computer or user policy based on a list of entries
// retrieved from a directory service.
func (m *Manager) ApplyPolicies(ctx context.Context, objectName string, isComputer bool, pols Policies) (err error) {
	defer decorate.OnError(&err, i18n.G("failed to apply policy to %q"), objectName)

	log.Infof(ctx, "Apply policy for %s (machine: %v)", objectName, isComputer)

	rules := pols.GetUniqueRules()
	var g errgroup.Group
	g.Go(func() error { return m.dconf.ApplyPolicy(ctx, objectName, isComputer, rules["dconf"]) })

	if !m.getSubcriptionState(ctx) {
		filterRules(ctx, rules)
	}

	g.Go(func() error { return m.privilege.ApplyPolicy(ctx, objectName, isComputer, rules["privilege"]) })
	// TODO g.Go(func() error { return m.scripts.ApplyPolicy(ctx, objectName, isComputer, rules["script"]) })
	// TODO g.Go(func() error { return m.apparmor.ApplyPolicy(ctx, objectName, isComputer, rules["apparmor"]) })
	if err := g.Wait(); err != nil {
		return err
	}

	if isComputer {
		// Apply GDM policy only now as we need dconf machine database to be ready first
		if err := m.gdm.ApplyPolicy(ctx, rules["gdm"]); err != nil {
			return err
		}
	}

	// Write cache Policies
	return pols.Save(filepath.Join(m.policiesCacheDir, objectName))
}

// DumpPolicies displays the currently applied policies and rules (since last update) for objectName.
// It can in addition show the rules and overridden content.
func (m *Manager) DumpPolicies(ctx context.Context, objectName string, withRules bool, withOverridden bool) (msg string, err error) {
	defer decorate.OnError(&err, i18n.G("failed to dump policies for %q"), objectName)

	log.Infof(ctx, "Dumping policies for %s", objectName)

	var out strings.Builder

	// Load machine for user
	// FIXME: fqdn in hostname?
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}

	var alreadyProcessedRules map[string]struct{}
	if objectName != hostname {
		fmt.Fprintln(&out, i18n.G("Policies from machine configuration:"))
		policiesHost, err := NewFromCache(filepath.Join(m.policiesCacheDir, hostname))
		if err != nil {
			return "", fmt.Errorf(i18n.G("no policy applied for %q: %v"), hostname, err)
		}
		for _, g := range policiesHost.GPOs {
			alreadyProcessedRules = g.Format(&out, withRules, withOverridden, alreadyProcessedRules)
		}
		fmt.Fprintln(&out, i18n.G("Policies from user configuration:"))
	}

	// Load target policies
	policiesTarget, err := NewFromCache(filepath.Join(m.policiesCacheDir, objectName))
	if err != nil {
		return "", fmt.Errorf(i18n.G("no policy applied for %q: %v"), objectName, err)
	}
	for _, g := range policiesTarget.GPOs {
		alreadyProcessedRules = g.Format(&out, withRules, withOverridden, alreadyProcessedRules)
	}

	return out.String(), nil
}

// LastUpdateFor returns the last update time for object or current machine.
func (m *Manager) LastUpdateFor(ctx context.Context, objectName string, isMachine bool) (t time.Time, err error) {
	defer decorate.OnError(&err, i18n.G("failed to get policy last update time %q (machine: %q)"), objectName, isMachine)

	log.Infof(ctx, "Get policies last update time %q (machine: %t)", objectName, isMachine)

	if isMachine {
		hostname, err := os.Hostname()
		if err != nil {
			return time.Time{}, err
		}
		objectName = hostname
	}

	info, err := os.Stat(filepath.Join(m.policiesCacheDir, objectName))
	if err != nil {
		return time.Time{}, fmt.Errorf(i18n.G("policies were not applied for %q: %v"), objectName, err)
	}
	return info.ModTime(), nil
}

// getSubcriptionState refresh subscription status from Ubuntu Advantage and return it.
func (m *Manager) getSubcriptionState(ctx context.Context) (subscriptionEnabled bool) {
	log.Debug(ctx, "Refresh subscription state")

	defer func() {
		m.Lock()
		m.subscriptionEnabled = subscriptionEnabled
		m.Unlock()

		if subscriptionEnabled {
			log.Debug(ctx, "Ubuntu advantage is enabled for GPO restrictions")
			return
		}

		log.Debug(ctx, "Ubuntu advantage is not enabled for GPO restrictions")
	}()

	// Check if the device is entitled to the Pro policy
	prop, err := m.subcriptionDbus.GetProperty(consts.SubcriptionDbusInterface + ".Status")
	if err != nil {
		log.Warningf(ctx, "no dbus connection to Ubuntu Advantage. Considering device as not enabled: %v", err)
		return false
	}
	enabled, ok := prop.Value().(string)
	if !ok {
		log.Warningf(ctx, "dbus returned an improper value from Ubuntu Advantage. Considering device as not enabled: %v", prop.Value())
		return false
	}

	if enabled != "enabled" {
		return false
	}

	return true
}

// filterRules allow to filter any rules that is not eligible for the current device.
func filterRules(ctx context.Context, rules map[string][]entry.Entry) {
	log.Debug(ctx, "Filtering Rules")

	rules["privilege"] = nil
	//rules["script"] = nil
}

// GetStatus returns dynamic part of our manager instance like subscription status.
func (m *Manager) GetStatus() (subscriptionEnabled bool) {
	m.RLock()
	defer m.RUnlock()
	return m.subscriptionEnabled
}
