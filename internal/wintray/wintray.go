// Package wintray implements a native Windows system-tray front-end for
// Trayscale. It reuses Trayscale's cross-platform tsutil core (which talks to
// the Tailscale LocalAPI) but replaces the Linux-only GTK4/libadwaita UI with a
// tray icon and menu built on fyne.io/systray.
package wintray

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image/png"
	"log/slog"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"deedles.dev/trayscale/internal/tsutil"
	"fyne.io/systray"
	ico "github.com/Kodeworks/golang-image-ico"
	"github.com/atotto/clipboard"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
)

//go:embed status-icon-active.png
var iconActivePNG []byte

//go:embed status-icon-inactive.png
var iconInactivePNG []byte

//go:embed status-icon-exit-node.png
var iconExitPNG []byte

// Caps on how many dynamic menu entries to show, to keep the menu usable even
// on very large tailnets.
const (
	maxExitNodes = 50
	maxPeers     = 100
)

// Run starts the tray application and blocks until it exits (either the user
// selects Quit or ctx is cancelled). It must be called from the main
// goroutine because systray runs the Win32 message loop there.
func Run(ctx context.Context) {
	a := &app{}
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.icoActive = pngToICO(iconActivePNG)
	a.icoInactive = pngToICO(iconInactivePNG)
	a.icoExit = pngToICO(iconExitPNG)
	a.latest = make(chan *tsutil.IPNStatus, 1)

	systray.Run(a.onReady, a.onExit)
}

type app struct {
	ctx    context.Context
	cancel context.CancelFunc

	icoActive, icoInactive, icoExit []byte

	poller tsutil.Poller
	latest chan *tsutil.IPNStatus // coalescing: only the newest matters

	mu          sync.Mutex
	status      *tsutil.IPNStatus
	exitTargets []tailcfg.StableNodeID // exit pool slot -> node id
	peerIPs     []string               // peer pool slot -> IPv4 string
	lastLog     string                 // last logged status summary (dedup)

	// Static menu items.
	mSelf         *systray.MenuItem
	mConnect      *systray.MenuItem
	mExit         *systray.MenuItem
	mExitNone     *systray.MenuItem
	mAllowLAN     *systray.MenuItem
	mAdvertise    *systray.MenuItem
	mAcceptRoutes *systray.MenuItem
	mAcceptDNS    *systray.MenuItem
	mPeers        *systray.MenuItem
	mDetails      *systray.MenuItem
	mAdmin        *systray.MenuItem
	mQuit         *systray.MenuItem

	exitPool *menuPool
	peerPool *menuPool
}

func (a *app) onReady() {
	systray.SetIcon(a.icoInactive)
	systray.SetTitle("Trayscale")
	systray.SetTooltip("Trayscale — starting…")

	a.mSelf = systray.AddMenuItem("This machine: (not connected)", "Copy this machine's Tailscale IP")
	a.mSelf.Disable()
	systray.AddSeparator()

	a.mConnect = systray.AddMenuItem("Connect", "Connect to / disconnect from the tailnet")

	a.mExit = systray.AddMenuItem("Exit node", "Route all traffic through another node")
	a.mExitNone = a.mExit.AddSubMenuItemCheckbox("None", "Do not use an exit node", true)
	a.exitPool = newMenuPool(func() *systray.MenuItem {
		return a.mExit.AddSubMenuItemCheckbox("", "", false)
	}, a.onExitClick)

	a.mAllowLAN = systray.AddMenuItemCheckbox("Allow local network access", "Keep LAN access while an exit node is in use", false)
	a.mAdvertise = systray.AddMenuItemCheckbox("Run as exit node", "Offer this machine as an exit node", false)
	a.mAcceptRoutes = systray.AddMenuItemCheckbox("Accept subnet routes", "Use subnet routes advertised by other nodes", false)
	a.mAcceptDNS = systray.AddMenuItemCheckbox("Accept DNS", "Use the tailnet's DNS configuration", false)

	systray.AddSeparator()
	a.mPeers = systray.AddMenuItem("Peers", "Other machines on your tailnet")
	a.peerPool = newMenuPool(func() *systray.MenuItem {
		return a.mPeers.AddSubMenuItem("", "")
	}, a.onPeerClick)

	systray.AddSeparator()
	a.mDetails = systray.AddMenuItem("Status details…", "Show connection details")
	a.mAdmin = systray.AddMenuItem("Admin console…", "Open the Tailscale admin console")
	systray.AddSeparator()
	a.mQuit = systray.AddMenuItem("Quit", "Exit Trayscale")

	// Left-clicking the tray icon shows details, mirroring the "Show" behavior
	// of the original app.
	systray.SetOnTapped(a.showDetails)

	a.wireStaticClicks()

	// Refresh reasonably quickly after actions so the menu feels responsive.
	a.poller.Interval = 2 * time.Second
	a.poller.New = a.onStatus
	go a.poller.Run(a.ctx)
	go a.updateLoop()
	go func() {
		<-a.ctx.Done()
		systray.Quit()
	}()
}

func (a *app) onExit() {
	a.cancel()
}

// onStatus is the poller callback. It runs on poller goroutines, so it only
// stashes the newest IPNStatus and lets updateLoop do the UI work.
func (a *app) onStatus(s tsutil.Status) {
	ipn, ok := s.(*tsutil.IPNStatus)
	if !ok {
		return
	}
	select {
	case a.latest <- ipn:
	default:
		// Drop the previous pending status and replace it with this one.
		select {
		case <-a.latest:
		default:
		}
		select {
		case a.latest <- ipn:
		default:
		}
	}
}

func (a *app) updateLoop() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case s := <-a.latest:
			a.rebuild(s)
		}
	}
}

func (a *app) wireStaticClicks() {
	watch := func(it *systray.MenuItem, f func()) {
		go func() {
			for range it.ClickedCh {
				f()
			}
		}()
	}

	watch(a.mSelf, func() { a.copySelfIP() })
	watch(a.mConnect, a.onConnectToggle)
	watch(a.mExitNone, func() { a.setExitNode("") })
	watch(a.mAllowLAN, func() {
		a.do("allow LAN access", func(ctx context.Context) error {
			return tsutil.AllowLANAccess(ctx, !a.mAllowLAN.Checked())
		})
	})
	watch(a.mAdvertise, func() {
		a.do("advertise exit node", func(ctx context.Context) error {
			return tsutil.AdvertiseExitNode(ctx, !a.mAdvertise.Checked())
		})
	})
	watch(a.mAcceptRoutes, func() {
		a.do("accept routes", func(ctx context.Context) error {
			return tsutil.AcceptRoutes(ctx, !a.mAcceptRoutes.Checked())
		})
	})
	watch(a.mAcceptDNS, func() {
		a.do("accept DNS", func(ctx context.Context) error {
			return tsutil.AcceptDNS(ctx, !a.mAcceptDNS.Checked())
		})
	})
	watch(a.mDetails, a.showDetails)
	watch(a.mAdmin, func() { openURL(tsutil.AdminDashboardURL) })
	watch(a.mQuit, func() { a.cancel() })
}

// rebuild refreshes the whole menu from the latest status. It only ever runs on
// the updateLoop goroutine, so menu mutations are serialized.
func (a *app) rebuild(s *tsutil.IPNStatus) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()

	online := s.Online()
	needsLogin := s.NeedsAuth()

	// Tray icon + tooltip.
	switch {
	case !online:
		systray.SetIcon(a.icoInactive)
	case s.ExitNodeActive():
		systray.SetIcon(a.icoExit)
	default:
		systray.SetIcon(a.icoActive)
	}
	systray.SetTooltip(a.tooltip(s))

	// Self machine.
	selfName, selfIP := a.selfInfo(s)
	if selfIP != "" {
		a.mSelf.SetTitle(fmt.Sprintf("This machine: %s (%s)", selfName, selfIP))
		a.mSelf.Enable()
	} else {
		a.mSelf.SetTitle("This machine: (not connected)")
		a.mSelf.Disable()
	}

	// Connect toggle.
	switch {
	case needsLogin:
		a.mConnect.SetTitle("Log in…")
	case online:
		a.mConnect.SetTitle("Disconnect")
	default:
		a.mConnect.SetTitle("Connect")
	}

	a.rebuildExitNodes(s, online)
	a.rebuildPrefsToggles(s, online)
	a.rebuildPeers(s)

	summary := fmt.Sprintf("state=%s self=%q ip=%s peers=%d exitActive=%t",
		stateText(s), selfName, selfIP, len(s.Peers), s.ExitNodeActive())
	if summary != a.lastLog {
		a.lastLog = summary
		slog.Info("status update", "summary", summary)
	}
}

func (a *app) rebuildExitNodes(s *tsutil.IPNStatus, online bool) {
	if online {
		a.mExit.Enable()
	} else {
		a.mExit.Disable()
	}

	currentExit := s.ExitNode()
	usingExit := s.ExitNodeActive()
	setChecked(a.mExitNone, !usingExit)

	type exitNode struct {
		id   tailcfg.StableNodeID
		name string
		cur  bool
	}
	var nodes []exitNode
	for id, peer := range s.Peers {
		if !tsaddr.ContainsExitRoutes(peer.AllowedIPs()) {
			continue
		}
		if tsutil.IsMullvad(peer) { // Mullvad nodes are numerous; omitted in v1.
			continue
		}
		nodes = append(nodes, exitNode{
			id:   id,
			name: peer.DisplayName(true),
			cur:  currentExit.Valid() && peer.StableID() == currentExit.StableID(),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].name < nodes[j].name })
	if len(nodes) > maxExitNodes {
		nodes = nodes[:maxExitNodes]
	}

	targets := make([]tailcfg.StableNodeID, len(nodes))
	for i, n := range nodes {
		it := a.exitPool.get(i)
		it.SetTitle(n.name)
		setChecked(it, n.cur)
		if online {
			it.Enable()
		} else {
			it.Disable()
		}
		it.Show()
		targets[i] = n.id
	}
	a.exitPool.hideFrom(len(nodes))

	a.mu.Lock()
	a.exitTargets = targets
	a.mu.Unlock()
}

func (a *app) rebuildPrefsToggles(s *tsutil.IPNStatus, online bool) {
	if !online || !s.Prefs.Valid() {
		for _, it := range []*systray.MenuItem{a.mAllowLAN, a.mAdvertise, a.mAcceptRoutes, a.mAcceptDNS} {
			it.Disable()
		}
		return
	}
	for _, it := range []*systray.MenuItem{a.mAllowLAN, a.mAdvertise, a.mAcceptRoutes, a.mAcceptDNS} {
		it.Enable()
	}
	setChecked(a.mAllowLAN, s.Prefs.ExitNodeAllowLANAccess())
	setChecked(a.mAdvertise, tsaddr.ContainsExitRoutes(s.Prefs.AdvertiseRoutes()))
	setChecked(a.mAcceptRoutes, s.Prefs.RouteAll())
	setChecked(a.mAcceptDNS, s.Prefs.CorpDNS())
}

func (a *app) rebuildPeers(s *tsutil.IPNStatus) {
	type peerInfo struct {
		title string
		ip    string
	}
	var peers []peerInfo
	for _, peer := range s.Peers {
		ip := peerIPv4(peer)
		title := peer.DisplayName(true)
		if ip != "" {
			title = fmt.Sprintf("%s  (%s)", title, ip)
		}
		peers = append(peers, peerInfo{title: title, ip: ip})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].title < peers[j].title })
	if len(peers) > maxPeers {
		peers = peers[:maxPeers]
	}

	if len(peers) == 0 {
		a.mPeers.Disable()
	} else {
		a.mPeers.Enable()
	}

	ips := make([]string, len(peers))
	for i, p := range peers {
		it := a.peerPool.get(i)
		it.SetTitle(p.title)
		it.SetTooltip("Copy " + p.ip)
		if p.ip == "" {
			it.Disable()
		} else {
			it.Enable()
		}
		it.Show()
		ips[i] = p.ip
	}
	a.peerPool.hideFrom(len(peers))

	a.mu.Lock()
	a.peerIPs = ips
	a.mu.Unlock()
}

// --- click handlers -------------------------------------------------------

func (a *app) onConnectToggle() {
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()
	if s == nil {
		return
	}

	switch {
	case s.NeedsAuth():
		a.do("log in", tsutil.StartLogin)
	case s.Online():
		a.do("disconnect", tsutil.Stop)
	default:
		a.do("connect", tsutil.Start)
	}
}

func (a *app) onExitClick(slot int) {
	a.mu.Lock()
	var id tailcfg.StableNodeID
	if slot < len(a.exitTargets) {
		id = a.exitTargets[slot]
	}
	a.mu.Unlock()
	if id == "" {
		return
	}
	a.setExitNode(id)
}

func (a *app) setExitNode(id tailcfg.StableNodeID) {
	a.do("set exit node", func(ctx context.Context) error {
		return tsutil.ExitNode(ctx, id)
	})
}

func (a *app) onPeerClick(slot int) {
	a.mu.Lock()
	var ip string
	if slot < len(a.peerIPs) {
		ip = a.peerIPs[slot]
	}
	a.mu.Unlock()
	if ip == "" {
		return
	}
	if err := clipboard.WriteAll(ip); err != nil {
		slog.Error("copy peer IP", "err", err)
	}
}

func (a *app) copySelfIP() {
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()
	if s == nil {
		return
	}
	addr := s.SelfAddr()
	if !addr.IsValid() {
		return
	}
	if err := clipboard.WriteAll(addr.String()); err != nil {
		slog.Error("copy self IP", "err", err)
	}
}

func (a *app) showDetails() {
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()

	var b strings.Builder
	if s == nil {
		b.WriteString("Waiting for Tailscale…")
	} else {
		name, ip := a.selfInfo(s)
		fmt.Fprintf(&b, "State: %s\n", stateText(s))
		if ip != "" {
			fmt.Fprintf(&b, "Machine: %s\nTailscale IP: %s\n", name, ip)
		}
		if s.ExitNodeActive() {
			ex := s.ExitNode()
			exName := "(unknown)"
			if ex.Valid() {
				exName = ex.DisplayName(true)
			}
			fmt.Fprintf(&b, "Exit node: %s\n", exName)
		}
		fmt.Fprintf(&b, "Peers: %d\n", len(s.Peers))
		if s.NetMap != nil && s.NetMap.Domain != "" {
			fmt.Fprintf(&b, "Tailnet: %s\n", s.NetMap.Domain)
		}
	}
	messageBox("Trayscale — Status", b.String())
}

// do runs an action against Tailscale off the UI goroutine with a timeout,
// logging any error.
func (a *app) do(name string, fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 45*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			slog.Error("action failed", "action", name, "err", err)
			messageBox("Trayscale — Error", fmt.Sprintf("Failed to %s:\n\n%v", name, err))
		}
	}()
}

// --- helpers --------------------------------------------------------------

func (a *app) tooltip(s *tsutil.IPNStatus) string {
	if !s.Online() {
		return "Trayscale — " + stateText(s)
	}
	name, ip := a.selfInfo(s)
	if ip == "" {
		return "Trayscale — Connected"
	}
	if s.ExitNodeActive() {
		return fmt.Sprintf("Trayscale — %s (%s) via exit node", name, ip)
	}
	return fmt.Sprintf("Trayscale — %s (%s)", name, ip)
}

func (a *app) selfInfo(s *tsutil.IPNStatus) (name, ip string) {
	if s.NetMap == nil {
		return "", ""
	}
	name = s.NetMap.SelfNode.DisplayName(true)
	if addr := s.SelfAddr(); addr.IsValid() {
		ip = addr.String()
	}
	return name, ip
}

func stateText(s *tsutil.IPNStatus) string {
	switch {
	case s.Online():
		return "Connected"
	case s.NeedsAuth():
		return "Needs login"
	default:
		return "Disconnected"
	}
}

func peerIPv4(peer tailcfg.NodeView) string {
	addrs := peer.Addresses()
	var first netip.Addr
	for i := range addrs.Len() {
		a := addrs.At(i).Addr()
		if !first.IsValid() {
			first = a
		}
		if a.Is4() {
			return a.String()
		}
	}
	if first.IsValid() {
		return first.String()
	}
	return ""
}

func setChecked(it *systray.MenuItem, checked bool) {
	if checked {
		it.Check()
	} else {
		it.Uncheck()
	}
}

func pngToICO(b []byte) []byte {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		panic(fmt.Errorf("decode tray icon: %w", err))
	}
	var buf bytes.Buffer
	if err := ico.Encode(&buf, img); err != nil {
		panic(fmt.Errorf("encode tray icon to ico: %w", err))
	}
	return buf.Bytes()
}

func openURL(url string) {
	// rundll32 is the most reliable way to open a URL in the default browser on
	// Windows without a console window flashing.
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		slog.Error("open url", "url", url, "err", err)
	}
}

// menuPool lazily creates and reuses tray menu items for a variable-length
// list. Items are created at the end of their parent menu on first use and
// hidden/relabeled on subsequent rebuilds.
type menuPool struct {
	make    func() *systray.MenuItem
	onClick func(slot int)
	items   []*systray.MenuItem
}

func newMenuPool(make func() *systray.MenuItem, onClick func(slot int)) *menuPool {
	return &menuPool{make: make, onClick: onClick}
}

func (p *menuPool) get(i int) *systray.MenuItem {
	for len(p.items) <= i {
		slot := len(p.items)
		it := p.make()
		it.Hide()
		p.items = append(p.items, it)
		go func(s int, ch chan struct{}) {
			for range ch {
				p.onClick(s)
			}
		}(slot, it.ClickedCh)
	}
	return p.items[i]
}

func (p *menuPool) hideFrom(n int) {
	for i := n; i < len(p.items); i++ {
		p.items[i].Hide()
	}
}
