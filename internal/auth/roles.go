package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Role is ordered: reader < operator < admin.
type Role int

const (
	RoleNone Role = iota
	RoleReader
	RoleOperator
	RoleAdmin
)

func (r Role) String() string { return [...]string{"none", "reader", "operator", "admin"}[r] }

func ParseRole(s string) Role {
	switch strings.ToLower(s) {
	case "reader":
		return RoleReader
	case "operator":
		return RoleOperator
	case "admin":
		return RoleAdmin
	}
	return RoleNone
}

// CapName is the Tailscale app capability granted in the ACL:
//
//	"grants": [{"src": ["tag:admin"], "dst": ["tag:k3s"], "app": {"vmd1.dev/cap/noded": [{"role": "operator"}]}}]
const CapName = "vmd1.dev/cap/noded"

// Identity is who is calling, as tailscaled reports it.
type Identity struct {
	Device string `json:"device"`
	User   string `json:"user"`
	Role   Role   `json:"-"`
}

// Whois resolves a peer address to an identity and role.
type Whois interface {
	Lookup(ctx context.Context, remote string) (Identity, error)
}

// LocalAPIWhois asks the local tailscaled (/localapi/v0/whois) who a tailnet address is and reads the capability grants.
type LocalAPIWhois struct{ Socket string }

func (w LocalAPIWhois) Lookup(ctx context.Context, remote string) (Identity, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", w.Socket)
		}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/whois?addr="+remote, nil)
	resp, err := c.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Identity{}, fmt.Errorf("whois: %s", resp.Status)
	}
	return ParseWhois(resp.Body)
}

type whoisDoc struct {
	Node struct {
		Name string `json:"Name"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
	CapMap map[string][]json.RawMessage `json:"CapMap"`
}

// ParseWhois turns the LocalAPI answer into an Identity; the highest role among the grants wins.
func ParseWhois(r io.Reader) (Identity, error) {
	var d whoisDoc
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return Identity{}, err
	}
	id := Identity{Device: strings.TrimSuffix(d.Node.Name, "."), User: d.UserProfile.LoginName}
	for _, raw := range d.CapMap[CapName] {
		var g struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &g) == nil {
			if rl := ParseRole(g.Role); rl > id.Role {
				id.Role = rl
			}
		}
	}
	return id, nil
}

// ---- policy ------------------------------------------------------------------------------------------------------------

type ctxKey struct{}

// FromContext returns the caller identity stored by the policy interceptor.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Mode is how roles are applied: audit logs the role and never denies; enforce denies calls above the caller's role.
type Mode string

const (
	ModeAudit   Mode = "audit"
	ModeEnforce Mode = "enforce"
)

// Policy resolves the caller and decides. Mode is read per call so a config change takes effect without a restart.
type Policy struct {
	Whois  Whois
	Mode   func() Mode
	Logf   func(format string, args ...any)
	limits sync.Map // caller+class -> *bucket
	Now    func() time.Time
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// rate per second and burst for a method class: reads are cheap, mutations are not.
func classLimits(required Role) (rate, burst float64) {
	if required >= RoleOperator {
		return 1, 5
	}
	return 10, 30
}

func (p *Policy) allow(key string, required Role) bool {
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	rate, burst := classLimits(required)
	v, _ := p.limits.LoadOrStore(key, &bucket{tokens: burst, last: now})
	b := v.(*bucket)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func newReqID() string { return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffffff) }

// Check resolves the caller, applies the role rule and the rate limit, logs the audit line and returns the context to use.
// outcome is called with the final result by the interceptor (see Done).
func (p *Policy) Check(ctx context.Context, method string, required Role) (context.Context, func(error), error) {
	pr, _ := peer.FromContext(ctx)
	remote := ""
	if pr != nil && pr.Addr != nil {
		remote = pr.Addr.String()
	}
	id, werr := p.Whois.Lookup(ctx, remote)
	rid := newReqID()
	mode := p.Mode()
	done := func(err error) {
		p.Logf("audit id=%s device=%q user=%q role=%s method=%s required=%s mode=%s outcome=%s", rid, id.Device, id.User, id.Role, method, required, mode, codeOf(err))
	}
	if werr != nil {
		// cannot identify the caller: in enforce mode that is a denial, in audit mode it is logged
		if mode == ModeEnforce {
			e := status.Errorf(codes.PermissionDenied, "cannot identify the caller: %v", werr)
			done(e)
			return ctx, func(error) {}, e
		}
		id = Identity{Device: "unknown", User: "unknown"}
	}
	if mode == ModeEnforce && id.Role < required {
		e := status.Errorf(codes.PermissionDenied, "%s needs the %s role; %s has %s (grant %q in the Tailscale ACL)", method, required, orUnknown(id.Device), id.Role, CapName)
		done(e)
		return ctx, func(error) {}, e
	}
	if !p.allow(id.Device+"|"+required.String(), required) {
		e := status.Error(codes.ResourceExhausted, "rate limit exceeded, slow down")
		done(e)
		return ctx, func(error) {}, e
	}
	return WithIdentity(ctx, id), done, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "this caller"
	}
	return s
}

func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	return status.Code(err).String()
}
