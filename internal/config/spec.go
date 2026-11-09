// Package config implements the declarative node configuration (EdgeNodeConfig): the typed schema, merging of the objects that
// select a node, validation, rendering into k3s / sysctl drop-ins, and the apply state machine (auto, staged, try).
package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Spec is the settings part of an EdgeNodeConfig object (the CRD schema in the homelab repo is the same shape).
// Pointers and nil maps mean "not set"; merging only overrides what is set.
type Spec struct {
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	Mode              string            `json:"mode,omitempty"` // auto (default) | staged | try
	TryTimeoutSeconds int               `json:"tryTimeoutSeconds,omitempty"`

	Kubelet              *Kubelet          `json:"kubelet,omitempty"`
	NodeLabels           map[string]string `json:"nodeLabels,omitempty"`
	DisableNetworkPolicy *bool             `json:"disableNetworkPolicy,omitempty"`
	K3sLogLevel          *int              `json:"k3sLogLevel,omitempty"`
	Sysctls              map[string]string `json:"sysctls,omitempty"`
	ModuleLockdown       *bool             `json:"moduleLockdown,omitempty"`
	TimeServers          []string          `json:"timeServers,omitempty"`
}

type Kubelet struct {
	SystemReserved map[string]string `json:"systemReserved,omitempty"` // cpu, memory, ephemeral-storage
	KubeReserved   map[string]string `json:"kubeReserved,omitempty"`
	EvictionHard   map[string]string `json:"evictionHard,omitempty"`
	MaxPods        *int              `json:"maxPods,omitempty"`
	ImageGCHigh    *int              `json:"imageGCHighThresholdPercent,omitempty"`
	ImageGCLow     *int              `json:"imageGCLowThresholdPercent,omitempty"`
}

// Object is one EdgeNodeConfig as read from the API.
type Object struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Spec       Spec   `json:"spec"`
}

var (
	quantityRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|Ki|Mi|Gi|%)?$`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	labelKeyRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValRe = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	intRe      = regexp.MustCompile(`^[0-9]{1,9}$`)
)

// SysctlAllowlist is the short list of host sysctls a config may set; everything else is rejected.
var SysctlAllowlist = map[string]bool{
	"vm.swappiness": true, "vm.vfs_cache_pressure": true, "vm.min_free_kbytes": true, "vm.page-cluster": true,
	"fs.inotify.max_user_instances": true, "fs.inotify.max_user_watches": true, "net.core.somaxconn": true,
	"net.ipv4.tcp_max_syn_backlog": true, "net.netfilter.nf_conntrack_max": true,
}

var evictionKeys = map[string]bool{"memory.available": true, "nodefs.available": true, "nodefs.inodesFree": true,
	"imagefs.available": true, "imagefs.inodesFree": true}
var reserveKeys = map[string]bool{"cpu": true, "memory": true, "ephemeral-storage": true}

// reservedLabelPrefixes can never be set through the config (a kubelet may not set them, or they are bootstrap items).
var reservedLabelPrefixes = []string{"kubernetes.io/", "k8s.io/", "node-role.kubernetes.io/", "node.kubernetes.io/", "node.vmd1.dev/"}
var reservedLabelKeys = map[string]bool{"node-type": true}

// Validate checks one effective Spec. It returns every problem, not just the first.
func (s *Spec) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	switch s.Mode {
	case "", "auto", "staged", "try":
	default:
		add("mode %q: want auto, staged or try", s.Mode)
	}
	if s.TryTimeoutSeconds < 0 || s.TryTimeoutSeconds > 3600 {
		add("tryTimeoutSeconds %d: want 0..3600", s.TryTimeoutSeconds)
	}
	if k := s.Kubelet; k != nil {
		checkMap := func(name string, m map[string]string, allowed map[string]bool) {
			for key, v := range m {
				if !allowed[key] {
					add("kubelet.%s: unknown key %q", name, key)
				}
				if !quantityRe.MatchString(v) {
					add("kubelet.%s[%s]: bad quantity %q", name, key, v)
				}
			}
		}
		checkMap("systemReserved", k.SystemReserved, reserveKeys)
		checkMap("kubeReserved", k.KubeReserved, reserveKeys)
		checkMap("evictionHard", k.EvictionHard, evictionKeys)
		if k.MaxPods != nil && (*k.MaxPods < 10 || *k.MaxPods > 250) {
			add("kubelet.maxPods %d: want 10..250", *k.MaxPods)
		}
		for n, v := range map[string]*int{"imageGCHighThresholdPercent": k.ImageGCHigh, "imageGCLowThresholdPercent": k.ImageGCLow} {
			if v != nil && (*v < 1 || *v > 100) {
				add("kubelet.%s %d: want 1..100", n, *v)
			}
		}
		if k.ImageGCHigh != nil && k.ImageGCLow != nil && *k.ImageGCLow >= *k.ImageGCHigh {
			add("kubelet: imageGCLowThresholdPercent must be below imageGCHighThresholdPercent")
		}
	}
	for key, v := range s.NodeLabels {
		if !labelKeyRe.MatchString(key) || !labelValRe.MatchString(v) {
			add("nodeLabels[%s]=%q: invalid label", key, v)
		}
		for _, p := range reservedLabelPrefixes {
			if strings.HasPrefix(key, p) {
				add("nodeLabels[%s]: prefix %q is reserved", key, p)
			}
		}
		if reservedLabelKeys[key] {
			add("nodeLabels[%s]: bootstrap label, cannot be changed", key)
		}
	}
	if s.K3sLogLevel != nil && (*s.K3sLogLevel < 0 || *s.K3sLogLevel > 9) {
		add("k3sLogLevel %d: want 0..9", *s.K3sLogLevel)
	}
	for key, v := range s.Sysctls {
		if !SysctlAllowlist[key] {
			add("sysctls[%s]: not on the allowlist", key)
		}
		if !intRe.MatchString(v) {
			add("sysctls[%s]=%q: want a non-negative integer", key, v)
		}
	}
	for _, h := range s.TimeServers {
		if !hostRe.MatchString(h) {
			add("timeServers: bad host %q", h)
		}
	}
	if len(s.TimeServers) > 4 {
		add("timeServers: at most 4")
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
}

// Selects reports whether the object's selector matches the node labels (an empty selector matches every node).
func (s *Spec) Selects(labels map[string]string) bool {
	for k, v := range s.NodeSelector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func mergeMap(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Merge returns the effective spec for a node: every selecting object, least specific first (fewest selector keys; ties
// broken by name), later ones overriding per field and per map key. The result is a fresh value.
func Merge(objs []Object, labels map[string]string) Spec {
	var sel []Object
	for _, o := range objs {
		if o.Spec.Selects(labels) {
			sel = append(sel, o)
		}
	}
	sort.SliceStable(sel, func(i, j int) bool {
		a, b := len(sel[i].Spec.NodeSelector), len(sel[j].Spec.NodeSelector)
		if a != b {
			return a < b
		}
		return sel[i].Name < sel[j].Name
	})
	var out Spec
	for _, o := range sel {
		Overlay(&out, o.Spec)
	}
	out.NodeSelector = nil
	return out
}

// Overlay applies s on top of out: set fields override, maps merge key by key.
func Overlay(out *Spec, s Spec) {
	if s.Mode != "" {
		out.Mode = s.Mode
	}
	if s.TryTimeoutSeconds != 0 {
		out.TryTimeoutSeconds = s.TryTimeoutSeconds
	}
	if k := s.Kubelet; k != nil {
		if out.Kubelet == nil {
			out.Kubelet = &Kubelet{}
		}
		out.Kubelet.SystemReserved = mergeMap(out.Kubelet.SystemReserved, k.SystemReserved)
		out.Kubelet.KubeReserved = mergeMap(out.Kubelet.KubeReserved, k.KubeReserved)
		out.Kubelet.EvictionHard = mergeMap(out.Kubelet.EvictionHard, k.EvictionHard)
		if k.MaxPods != nil {
			out.Kubelet.MaxPods = k.MaxPods
		}
		if k.ImageGCHigh != nil {
			out.Kubelet.ImageGCHigh = k.ImageGCHigh
		}
		if k.ImageGCLow != nil {
			out.Kubelet.ImageGCLow = k.ImageGCLow
		}
	}
	out.NodeLabels = mergeMap(out.NodeLabels, s.NodeLabels)
	out.Sysctls = mergeMap(out.Sysctls, s.Sysctls)
	if s.DisableNetworkPolicy != nil {
		out.DisableNetworkPolicy = s.DisableNetworkPolicy
	}
	if s.K3sLogLevel != nil {
		out.K3sLogLevel = s.K3sLogLevel
	}
	if s.ModuleLockdown != nil {
		out.ModuleLockdown = s.ModuleLockdown
	}
	if len(s.TimeServers) > 0 {
		out.TimeServers = append([]string(nil), s.TimeServers...)
	}
}

// Hash is a stable identity of the effective settings (mode and timeout are about how to apply, not what, so they are excluded).
func (s Spec) Hash() string {
	c := s
	c.Mode, c.TryTimeoutSeconds, c.NodeSelector = "", 0, nil
	b, _ := json.Marshal(c) // maps are marshalled with sorted keys
	return fmt.Sprintf("%x", fnv64(b))
}

func fnv64(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// IsZero reports whether nothing is configured (the image defaults apply).
func (s Spec) IsZero() bool { return s.Hash() == (Spec{}).Hash() }
