package tsutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/cmd/tailscale/cli"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/netmon"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
	"tailscale.com/util/eventbus"
)

var (
	localClient    local.Client
	bus            = eventbus.New()
	monitor        = initMonitor()
	netcheckClient = netcheck.Client{
		NetMon: monitor,
		Logf:   logger.Discard,
	}
)

func initMonitor() *netmon.Monitor {
	monitor, err := netmon.New(bus, logger.Discard)
	if err != nil {
		slog.Error("init netmon monitor", "err", err)
	}
	return monitor
}

// GetStatus returns the status of the connection to the Tailscale
// network. If the network is not currently connected, it returns
// nil, nil.
func GetStatus(ctx context.Context) (*ipnstate.Status, error) {
	st, err := localClient.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tailscale status: %w", err)
	}
	return st, nil
}

// Prefs returns the options of the local node.
func Prefs(ctx context.Context) (*ipn.Prefs, error) {
	return localClient.GetPrefs(ctx)
}

// Start connects the local peer to the Tailscale network.
func Start(ctx context.Context) error {
	return cli.RunWithContext(ctx, []string{"up"})
}

// Stop disconnects the local peer from the Tailscale network.
func Stop(ctx context.Context) error {
	return cli.RunWithContext(ctx, []string{"down"})
}

// ExitNode uses the specified peer as an exit node, or unsets
// an existing exit node if peer is an empty string.
func ExitNode(ctx context.Context, peer tailcfg.StableNodeID) error {
	if peer == "" {
		var prefs ipn.Prefs
		prefs.ClearExitNode()
		_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs:         prefs,
			ExitNodeIDSet: true,
			ExitNodeIPSet: true,
		})
		if err != nil {
			return fmt.Errorf("edit prefs: %w", err)
		}
		return nil
	}

	prefs := ipn.Prefs{
		ExitNodeID: peer,
	}
	_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:         prefs,
		ExitNodeIDSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

func SetUseExitNode(ctx context.Context, use bool) error {
	useErr := localClient.SetUseExitNode(ctx, use)
	if useErr == nil {
		return nil
	}

	suggested, suggestErr := localClient.SuggestExitNode(ctx)
	if suggestErr == nil {
		slog.Info("got suggested exit node", "id", suggested.ID, "name", suggested.Name, "location", suggested.Location)
		suggestErr = ExitNode(ctx, suggested.ID)
		if suggestErr == nil {
			return nil
		}
	}

	return errors.Join(useErr, suggestErr)
}

// AdvertiseExitNode enables and disables exit node advertisement for
// the current node.
func AdvertiseExitNode(ctx context.Context, enable bool) error {
	var prefs ipn.Prefs
	prefs.SetAdvertiseExitNode(enable)

	_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:              prefs,
		AdvertiseRoutesSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

func AdvertiseRoutes(ctx context.Context, routes []netip.Prefix) error {
	prefs, err := Prefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}
	exit := prefs.AdvertisesExitNode()
	prefs.AdvertiseRoutes = routes
	prefs.SetAdvertiseExitNode(exit)

	_, err = localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:              *prefs,
		AdvertiseRoutesSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

// AllowLANAccess enables and disables the ability for the current
// node to get access to the regular LAN that it is connected to while
// an exit node is in use.
func AllowLANAccess(ctx context.Context, allow bool) error {
	prefs := ipn.Prefs{
		ExitNodeAllowLANAccess: allow,
	}

	_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:                     prefs,
		ExitNodeAllowLANAccessSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

// AcceptRoutes sets whether or not all shared subnet routes from
// other nodes should be used by the local node.
func AcceptRoutes(ctx context.Context, accept bool) error {
	prefs := ipn.Prefs{
		RouteAll: accept,
	}

	_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:       prefs,
		RouteAllSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

// AcceptDNS sets whether or not the Tailscale DNS config should be
// used.
func AcceptDNS(ctx context.Context, accept bool) error {
	prefs := ipn.Prefs{
		CorpDNS: accept,
	}

	_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:      prefs,
		CorpDNSSet: true,
	})
	if err != nil {
		return fmt.Errorf("edit prefs: %w", err)
	}

	return nil
}

// SetControlURL changes the URL of the control plane server used by
// the daemon. If controlURL is empty, the default Tailscale server is
// used.
func SetControlURL(ctx context.Context, controlURL string) error {
	prefs, err := Prefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}
	prefs.ControlURL = controlURL

	err = localClient.Start(ctx, ipn.Options{
		UpdatePrefs: prefs,
	})
	if err != nil {
		return fmt.Errorf("start local client: %w", err)
	}

	return nil
}

func NetCheck(ctx context.Context, full bool) (*netcheck.Report, *tailcfg.DERPMap, error) {
	err := netcheckClient.Standalone(ctx, "")
	if err != nil {
		return nil, nil, fmt.Errorf("standalone: %w", err)
	}

	dm, err := localClient.CurrentDERPMap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("current DERP map: %w", err)
	}

	if full {
		netcheckClient.MakeNextReportFull()
	}
	r, err := netcheckClient.GetReport(ctx, dm, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("get netcheck report: %w", err)
	}

	return r, dm, nil
}

func PushFile(ctx context.Context, target tailcfg.StableNodeID, size int64, name string, r io.Reader) error {
	return localClient.PushFile(ctx, target, size, name, r)
}

func GetWaitingFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	return localClient.GetWaitingFile(ctx, name)
}

func DeleteWaitingFile(ctx context.Context, name string) error {
	return localClient.DeleteWaitingFile(ctx, name)
}

// WaitingFiles polls for any pending incoming files. It returns
// quickly if there are no files currently pending.
func WaitingFiles(ctx context.Context) ([]apitype.WaitingFile, error) {
	// TODO: https://github.com/tailscale/tailscale/issues/8911
	return localClient.AwaitWaitingFiles(ctx, time.Second)
}

func FileTargets(ctx context.Context) ([]apitype.FileTarget, error) {
	return localClient.FileTargets(ctx)
}

func GetProfileStatus(ctx context.Context) (ipn.LoginProfile, []ipn.LoginProfile, error) {
	return localClient.ProfileStatus(ctx)
}

func SwitchProfile(ctx context.Context, id ipn.ProfileID) error {
	return localClient.SwitchProfile(ctx, id)
}

func StartLogin(ctx context.Context) error {
	return localClient.StartLoginInteractive(ctx)
}

// Ping pings a peer at the given Tailscale IP and returns the result, including
// the round-trip latency and whether the connection was direct or via DERP.
func Ping(ctx context.Context, ip netip.Addr, pingType tailcfg.PingType) (*ipnstate.PingResult, error) {
	return localClient.Ping(ctx, ip, pingType)
}

// PingReport is the parsed result of a single ping.
type PingReport struct {
	LatencyMs float64
	Direct    bool   // true if a direct (non-DERP) path was used
	Relay     string // DERP region code if relayed, else ""
}

var pingLatencyRe = regexp.MustCompile(`in ([0-9.]+)\s*ms`)

func tailscaleExe() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Tailscale\tailscale.exe`,
			`C:\Program Files (x86)\Tailscale\tailscale.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return "tailscale"
}

// PingOnce pings a peer once by shelling out to the tailscale CLI in a fresh
// process. The in-process LocalAPI ping can wedge in this long-lived app (it
// times out even on a dedicated client) while a fresh CLI process is reliable,
// so this is the robust path for the live ping graph.
func PingOnce(ctx context.Context, ip netip.Addr) (PingReport, error) {
	// --until-direct=false returns on the first pong (fast, ~1s) instead of
	// waiting up to ~15s to negotiate a direct path — matching the macOS live
	// ping, which shows DERP-relayed first and upgrades to Direct as it warms up.
	cmd := exec.CommandContext(ctx, tailscaleExe(), "ping", "--c", "1", "--until-direct=false", "--timeout", "6s", ip.String())
	hideCmdWindow(cmd)
	out, err := cmd.CombinedOutput()
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if !strings.Contains(line, "pong") {
		if line == "" {
			if err != nil {
				return PingReport{}, err
			}
			return PingReport{}, fmt.Errorf("no response")
		}
		return PingReport{}, fmt.Errorf("%s", line)
	}
	m := pingLatencyRe.FindStringSubmatch(line)
	if m == nil {
		return PingReport{}, fmt.Errorf("%s", line)
	}
	ms, _ := strconv.ParseFloat(m[1], 64)
	rep := PingReport{LatencyMs: ms}
	if i := strings.Index(line, "via DERP("); i >= 0 {
		rest := line[i+len("via DERP("):]
		if j := strings.IndexByte(rest, ')'); j >= 0 {
			rep.Relay = rest[:j]
		}
	} else {
		rep.Direct = true
	}
	return rep, nil
}
