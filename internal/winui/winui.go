// Package winui implements a native Windows GUI window for Trayscale using Gio
// (gioui.org), a pure-Go immediate-mode toolkit. It reuses Trayscale's tsutil
// core (Tailscale LocalAPI) and reproduces the upstream libadwaita window:
// a searchable, owner-grouped sidebar of machines and a detail pane with IPs,
// option toggles, incoming files, advertised routes and a network check.
package winui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/io/clipboard"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"deedles.dev/trayscale/internal/tsutil"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
)

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: byte(v >> 16), G: byte(v >> 8), B: byte(v), A: 0xFF}
}

var (
	colBg         = rgb(0xF6F5F4)
	colCard       = rgb(0xFFFFFF)
	colBorder     = rgb(0xDCDAD7)
	colText       = rgb(0x2A2A2A)
	colSub        = rgb(0x8C8C8C)
	colPink       = rgb(0xE0338A)
	colGreen      = rgb(0x5AC85A)
	colRed        = rgb(0xE8736F)
	colBlue       = rgb(0x5B9BD5)
	colYellow     = rgb(0xF2C744)
	colWhite      = rgb(0xFFFFFF)
	colSel        = rgb(0xEDEBFA)
	colHeaderB    = rgb(0xEFEDEB)
	colOrange     = rgb(0xE8730F)
	colLine       = rgb(0x3B7DED)
	colOfflineDot = rgb(0xB0B4B9)
)

// Run opens the window and blocks until it is closed or ctx is cancelled.
func Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	u := &ui{ctx: ctx}
	u.th = material.NewTheme()
	u.th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	u.th.Palette.Bg = colBg
	u.th.Palette.Fg = colText
	u.th.Palette.ContrastBg = colPink
	u.th.Palette.ContrastFg = colWhite
	u.sbList.Axis = layout.Vertical
	u.detailList.Axis = layout.Vertical
	u.search.SingleLine = true
	u.routeEditor.SingleLine = true
	u.selKey = keySelf

	w := new(app.Window)
	w.Option(app.Title("Trayscale"), app.Size(unit.Dp(1200), unit.Dp(820)), app.MinSize(unit.Dp(680), unit.Dp(480)))
	u.w = w

	u.poller.Interval = 2 * time.Second
	u.poller.New = u.onStatus
	go u.poller.Run(ctx)
	go func() {
		<-ctx.Done()
		w.Perform(system.ActionClose)
	}()
	// Periodic redraw guarantees the first status is picked up (even if its
	// Invalidate raced the initial frame) and keeps online indicators fresh.
	go func() {
		t := time.NewTicker(750 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.Invalidate()
			}
		}
	}()

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			cancel()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			u.frame(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

const (
	keySelf    = "\x00self"
	keyMullvad = "\x00mullvad"
)

const (
	kindSelf = iota
	kindMullvad
	kindPeer
	kindHeader
)

type ui struct {
	ctx    context.Context
	w      *app.Window
	th     *material.Theme
	poller tsutil.Poller

	mu       sync.Mutex
	status   *tsutil.IPNStatus
	files    []apitype.WaitingFile
	net      *netResult
	dirty    bool
	profile  string
	exitName string // current exit node display name, "" if none

	allItems    []sbItem
	byKey       map[string]*sbItem
	selKey      string
	syncedTo    string
	lastPingSel string // selKey the current ping session belongs to

	// navigation
	view       int // 0 = Devices, 1 = Exit Nodes
	navDevices widget.Clickable
	navExit    widget.Clickable

	// header
	connect     widget.Bool
	exitNodeBtn widget.Clickable

	// exit nodes view
	exitList     widget.List
	exitNoneBtn  widget.Clickable
	exitRows     []widget.Clickable
	exitAllowLAN widget.Bool
	exitIDs      []tailcfg.StableNodeID

	// taildrop
	sendFileBtn widget.Clickable

	// ping
	pingBtn     widget.Clickable
	pingKey     string    // peer key the results belong to
	pingResults []float64 // recent latencies in ms
	pinging     bool
	pingCancel  context.CancelFunc
	pingGen     int
	pingConn    string // "Direct connection" / "DERP-relayed connection"
	pingDirect  bool
	pingErr     string

	// options
	optAdvertise    widget.Bool
	optAllowLAN     widget.Bool
	optAcceptRoutes widget.Bool
	optAcceptDNS    widget.Bool
	peerExit        widget.Bool

	// sidebar
	search widget.Editor
	sbList widget.List
	rows   []widget.Clickable

	// detail
	detailList   widget.List
	copyBtns     []widget.Clickable
	routeRmBtns  []widget.Clickable
	fileDelBtns  []widget.Clickable
	routeEditor  widget.Editor
	routeAddBtn  widget.Clickable
	routeShowBtn widget.Clickable
	showRouteBox bool
	netCheckBtn  widget.Clickable
	netRunning   bool
}

type sbItem struct {
	key     string
	kind    int
	id      tailcfg.StableNodeID
	name    string
	sub     string
	title   string
	dnsName string
	ips     []string
	addrs   []addrEntry
	online  bool
	avatar  color.NRGBA
	glyph   string

	osName     string
	keyExpiry  string
	created    string
	lastSeen   string
	fileTarget bool

	exitOption bool
	isExitNode bool

	hdrOnline int // for kindHeader: number of online devices in the group
	hdrTotal  int // for kindHeader: total devices in the group
}

type addrEntry struct {
	value string
	label string
}

type netResult struct {
	when time.Time
	udp  bool
	ipv4 string
	ipv6 string
	derp string
	lats []latEntry
}

type latEntry struct {
	name string
	dur  time.Duration
}

func (u *ui) onStatus(s tsutil.Status) {
	switch s := s.(type) {
	case *tsutil.IPNStatus:
		u.mu.Lock()
		u.status = s
		u.dirty = true
		u.mu.Unlock()
	case *tsutil.FileStatus:
		u.mu.Lock()
		u.files = s.Files
		u.mu.Unlock()
	default:
		return
	}
	u.w.Invalidate()
}

func (u *ui) frame(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	s := u.status
	dirty := u.dirty
	u.dirty = false
	u.mu.Unlock()

	if dirty && s != nil {
		u.rebuildModel(s)
		u.syncFromStatus(s)
	}

	if u.connect.Update(gtx) {
		on := u.connect.Value
		u.do(func(ctx context.Context) error {
			if on {
				return tsutil.Start(ctx)
			}
			return tsutil.Stop(ctx)
		})
	}
	if u.exitNodeBtn.Clicked(gtx) {
		// Clicking the header exit-node chip clears any active exit node.
		u.do(func(ctx context.Context) error { return tsutil.ExitNode(ctx, "") })
	}
	if u.navDevices.Clicked(gtx) {
		u.view = 0
	}
	if u.navExit.Clicked(gtx) {
		u.view = 1
	}

	paint.Fill(gtx.Ops, colBg)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.layoutHeader),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }),
		layout.Flexed(1, u.layoutBody),
	)
}

func (u *ui) layoutHeader(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	profile := u.profile
	exitName := u.exitName
	u.mu.Unlock()
	if profile == "" {
		profile = "Not connected"
	}
	exitLabel := "(None)"
	if exitName != "" {
		exitLabel = exitName
	}

	return layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(material.Switch(u.th, &u.connect, "Connected").Layout),
			layout.Rigid(layout.Spacer{Width: 14}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(u.th, profile)
				l.Font.Weight = font.Bold
				l.MaxLines = 1
				return l.Layout(gtx)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(u.th, "Exit node:")
				l.Color = colSub
				return layout.Inset{Right: 8}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.exitNodeBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, func(gtx layout.Context) layout.Dimensions {
						return material.Body2(u.th, exitLabel).Layout(gtx)
					})
				})
			}),
		)
	})
}

func (u *ui) layoutBody(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = gtx.Dp(170)
			gtx.Constraints.Min.X = gtx.Dp(170)
			return u.layoutRail(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, false) }),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if u.view == 1 {
				return u.layoutExitNodes(gtx)
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Max.X = gtx.Dp(300)
					gtx.Constraints.Min.X = gtx.Dp(300)
					return u.layoutSidebar(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, false) }),
				layout.Flexed(1, u.layoutDetail),
			)
		}),
	)
}

func (u *ui) layoutRail(gtx layout.Context) layout.Dimensions {
	item := func(gtx layout.Context, btn *widget.Clickable, label string, selected bool) layout.Dimensions {
		return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					if selected {
						fillRRect(gtx, gtx.Constraints.Min, gtx.Dp(8), colSel)
					}
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(u.th, label)
						if selected {
							l.Font.Weight = font.Bold
						}
						return l.Layout(gtx)
					})
				}),
			)
		})
	}
	return layout.Inset{Top: 8, Left: 8, Right: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return item(gtx, &u.navDevices, "Devices", u.view == 0) }),
			layout.Rigid(layout.Spacer{Height: 4}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return item(gtx, &u.navExit, "Exit Nodes", u.view == 1) }),
		)
	})
}

// visibleItems applies the search filter, dropping group headers whose members
// all got filtered out.
func (u *ui) visibleItems() []*sbItem {
	q := strings.ToLower(strings.TrimSpace(u.search.Text()))
	out := make([]*sbItem, 0, len(u.allItems))
	for i := range u.allItems {
		it := &u.allItems[i]
		if it.kind == kindHeader {
			out = append(out, it) // pruned below
			continue
		}
		if q != "" && it.kind == kindPeer && !strings.Contains(strings.ToLower(it.name), q) {
			continue
		}
		out = append(out, it)
	}
	// Prune headers with no following non-header item.
	pruned := out[:0]
	for i := 0; i < len(out); i++ {
		if out[i].kind == kindHeader {
			if i+1 >= len(out) || out[i+1].kind == kindHeader {
				continue
			}
		}
		pruned = append(pruned, out[i])
	}
	return pruned
}

func (u *ui) layoutSidebar(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 8, Bottom: 8, Left: 10, Right: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return chip(gtx, func(gtx layout.Context) layout.Dimensions {
					ed := material.Editor(u.th, &u.search, "Search machines…")
					ed.TextSize = unit.Sp(14)
					return ed.Layout(gtx)
				})
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			items := u.visibleItems()
			if len(u.rows) < len(items) {
				u.rows = make([]widget.Clickable, len(items))
			}
			return material.List(u.th, &u.sbList).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
				it := items[i]
				if it.kind == kindHeader {
					return u.layoutGroupHeader(gtx, it)
				}
				if u.rows[i].Clicked(gtx) {
					u.selKey = it.key
				}
				return u.rows[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.layoutSidebarRow(gtx, it, it.key == u.selKey)
				})
			})
		}),
	)
}

func (u *ui) layoutGroupHeader(gtx layout.Context, it *sbItem) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return layout.Inset{Top: 10, Bottom: 4, Left: 14, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(u.th, strings.ToUpper(it.title))
				l.Color = colSub
				l.Font.Weight = font.Bold
				l.MaxLines = 1
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(u.th, fmt.Sprintf("%d online", it.hdrOnline))
				l.Color = colSub
				if it.hdrOnline > 0 {
					l.Color = colGreen
				}
				return l.Layout(gtx)
			}),
		)
	})
}

func (u *ui) layoutSidebarRow(gtx layout.Context, it *sbItem, selected bool) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			if selected {
				fillRRect(gtx, gtx.Constraints.Min, gtx.Dp(8), colSel)
			}
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Top: 8, Bottom: 8, Left: 14, Right: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// macOS-style: a small online dot inline with the name.
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.statusDot(gtx, it.online) }),
							layout.Rigid(layout.Spacer{Width: 9}.Layout),
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								l := material.Body1(u.th, it.name)
								l.MaxLines = 1
								l.Font.Weight = font.Medium
								return l.Layout(gtx)
							}),
						)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if it.sub == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: 1}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							l := material.Caption(u.th, it.sub)
							l.Color = colSub
							l.MaxLines = 1
							return l.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (u *ui) layoutExitNodes(gtx layout.Context) layout.Dimensions {
	var exits []*sbItem
	for i := range u.allItems {
		it := &u.allItems[i]
		if it.kind == kindPeer && it.exitOption {
			exits = append(exits, it)
		}
	}
	sort.Slice(exits, func(i, j int) bool { return strings.ToLower(exits[i].name) < strings.ToLower(exits[j].name) })

	u.exitIDs = u.exitIDs[:0]
	for _, e := range exits {
		u.exitIDs = append(u.exitIDs, e.id)
	}
	if len(u.exitRows) < len(exits) {
		u.exitRows = make([]widget.Clickable, len(exits))
	}

	// Handle selections.
	if u.exitNoneBtn.Clicked(gtx) {
		u.do(func(ctx context.Context) error { return tsutil.ExitNode(ctx, "") })
	}
	for i := range exits {
		if u.exitRows[i].Clicked(gtx) {
			id := u.exitIDs[i]
			u.do(func(ctx context.Context) error { return tsutil.ExitNode(ctx, id) })
		}
	}
	if u.exitAllowLAN.Update(gtx) {
		v := u.exitAllowLAN.Value
		u.do(func(ctx context.Context) error { return tsutil.AllowLANAccess(ctx, v) })
	}

	u.mu.Lock()
	anyActive := u.exitName != ""
	u.mu.Unlock()

	exitRow := func(gtx layout.Context, btn *widget.Clickable, label string, active bool) layout.Dimensions {
		return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, material.Body1(u.th, label).Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if !active {
							return layout.Dimensions{}
						}
						l := material.Body1(u.th, "✓")
						l.Color = colPink
						l.Font.Weight = font.Bold
						return l.Layout(gtx)
					}),
				)
			})
		})
	}

	return material.List(u.th, &u.exitList).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Inset{Top: 20, Bottom: 24, Left: 28, Right: 28}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			var kids []layout.FlexChild
			kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.H5(u.th, "Exit Nodes").Layout(gtx)
			}))
			kids = append(kids, layout.Rigid(layout.Spacer{Height: 16}.Layout))
			kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
					rows := []layout.FlexChild{
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return exitRow(gtx, &u.exitNoneBtn, "None", !anyActive)
						}),
					}
					for i, e := range exits {
						i, e := i, e
						rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
						rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return exitRow(gtx, &u.exitRows[i], e.name, e.isExitNode)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
				})
			}))
			if len(exits) == 0 {
				kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, "No exit nodes are available on this tailnet.")
					l.Color = colSub
					return layout.Inset{Top: 12}.Layout(gtx, l.Layout)
				}))
			}
			kids = append(kids, layout.Rigid(layout.Spacer{Height: 20}.Layout))
			kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.switchRow(gtx, "Allow local network access", "Keep LAN access while an exit node is in use", &u.exitAllowLAN)
				})
			}))
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
		})
	})
}

func (u *ui) layoutDetail(gtx layout.Context) layout.Dimensions {
	it := u.selected()
	if it == nil {
		return u.centerMsg(gtx, "Waiting for Tailscale…")
	}
	if u.syncedTo != u.selKey {
		u.syncedTo = u.selKey
		u.peerExit.Value = it.isExitNode
	}
	// Only stop the ping when the *selected machine* actually changes — not on
	// every status poll (which resets syncedTo to re-sync the exit-node switch).
	if u.lastPingSel != u.selKey {
		u.lastPingSel = u.selKey
		u.mu.Lock()
		if u.pingCancel != nil {
			u.pingCancel()
			u.pingCancel = nil
		}
		u.pingGen++
		u.pinging = false
		u.pingKey = ""
		u.pingResults = nil
		u.pingConn = ""
		u.pingErr = ""
		u.mu.Unlock()
	}
	u.handleDetailInputs(gtx, it)

	// One scrollable column so long detail pages (self page) never clip.
	return material.List(u.th, &u.detailList).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Inset{Top: 20, Bottom: 24, Left: 28, Right: 28}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.detailContent(gtx, it)
		})
	})
}

func (u *ui) detailContent(gtx layout.Context, it *sbItem) layout.Dimensions {
	var kids []layout.FlexChild
	add := func(w layout.Widget) { kids = append(kids, layout.Rigid(w)) }
	gap := func(h int) { kids = append(kids, layout.Rigid(layout.Spacer{Height: unit.Dp(h)}.Layout)) }

	add(func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		h := material.H5(u.th, it.title)
		h.Alignment = text.Middle
		h.MaxLines = 1
		return h.Layout(gtx)
	})
	if it.kind != kindMullvad {
		gap(6)
		add(func(gtx layout.Context) layout.Dimensions {
			statusTxt := "Not Connected"
			dot := colSub
			if it.online {
				statusTxt = "Connected"
				dot = colGreen
			}
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceSides}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					d := material.Body2(u.th, "●")
					d.Color = dot
					return d.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, statusTxt)
					l.Color = colSub
					return l.Layout(gtx)
				}),
			)
		})
	}
	gap(22)

	if it.kind == kindMullvad {
		add(func(gtx layout.Context) layout.Dimensions {
			l := material.Body1(u.th, "Mullvad exit nodes appear in the sidebar. Select one to route through it.")
			l.Color = colSub
			return l.Layout(gtx)
		})
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	}

	// Tailscale addresses (MagicDNS + IPv4/IPv6, macOS style).
	if len(it.addrs) > 0 {
		add(func(gtx layout.Context) layout.Dimensions { return u.sectionLabel(gtx, "Tailscale addresses", nil) })
		gap(8)
		addrs := it.addrs
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
				var rows []layout.FlexChild
				for idx, a := range addrs {
					a, idx := a, idx
					if idx > 0 {
						rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
					}
					rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.addrRow(gtx, a, idx) }))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
			})
		})
		gap(22)
	}

	// Options.
	add(func(gtx layout.Context) layout.Dimensions { return u.sectionLabel(gtx, "Options", nil) })
	gap(8)
	add(func(gtx layout.Context) layout.Dimensions {
		return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
			if it.kind == kindSelf {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.switchRow(gtx, "Advertise exit node", "Allow this machine to be used as an exit node", &u.optAdvertise)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.switchRow(gtx, "Allow LAN access", "Bypass Tailscale routing for local network access", &u.optAllowLAN)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.switchRow(gtx, "Accept routes", "Use exported routes from other machines", &u.optAcceptRoutes)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.switchRow(gtx, "Accept DNS", "Use Tailscale DNS configuration", &u.optAcceptDNS)
					}),
				)
			}
			if it.exitOption {
				return u.switchRow(gtx, "Use as exit node", "Route this machine's traffic through "+it.title, &u.peerExit)
			}
			return u.textRow(gtx, "No options available for this machine.")
		})
	})

	// Taildrop send (peers that can receive files).
	if it.kind == kindPeer && it.fileTarget {
		gap(22)
		add(func(gtx layout.Context) layout.Dimensions { return u.sectionLabel(gtx, "Taildrop", nil) })
		gap(8)
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 16, Bottom: 16, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Body2(u.th, "Send a file to this device")
							l.Color = colSub
							return l.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Height: 10}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.sendFileBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, func(gtx layout.Context) layout.Dimensions {
									l := material.Body2(u.th, "Select a File…")
									l.Color = colPink
									return l.Layout(gtx)
								})
							})
						}),
					)
				})
			})
		})
	}

	// Ping (peers) — macOS-style: continuous ping, connection type + live graph.
	if it.kind == kindPeer {
		u.mu.Lock()
		results := append([]float64(nil), u.pingResults...)
		pinging := u.pinging && u.pingKey == it.key
		conn := u.pingConn
		pingErr := u.pingErr
		sameKey := u.pingKey == it.key
		u.mu.Unlock()
		if !sameKey {
			results = nil
			conn = ""
			pingErr = ""
		}
		latest := "—"
		if len(results) > 0 {
			latest = fmt.Sprintf("%.0f ms", results[len(results)-1])
		}

		gap(22)
		// Header: "Ping" + Stop / Ping device action on the right.
		add(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(u.th, "Ping")
					l.Font.Weight = font.Bold
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !pinging {
						return layout.Dimensions{} // idle: button lives centered in the graph
					}
					return u.pingBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, func(gtx layout.Context) layout.Dimensions {
							l := material.Body2(u.th, "Stop")
							l.Color = colLine
							return l.Layout(gtx)
						})
					})
				}),
			)
		})
		gap(8)
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									glyph, col, txt := "", colSub, "Not pinged yet"
									if pinging && conn == "" {
										txt = "Pinging…"
									}
									if pingErr != "" && conn == "" {
										col, txt = colOrange, "Ping failed: "+pingErr
									}
									switch conn {
									case "Direct connection":
										glyph, col, txt = "→ ", colGreen, conn
									case "DERP-relayed connection":
										glyph, col, txt = "↱ ", colOrange, conn
									}
									l := material.Body1(u.th, glyph+txt)
									l.Color = col
									return l.Layout(gtx)
								}),
								layout.Flexed(1, layout.Spacer{}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									l := material.Body1(u.th, latest)
									l.Font.Weight = font.Bold
									return l.Layout(gtx)
								}),
							)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: 12, Bottom: 12, Left: 12, Right: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Stack{Alignment: layout.Center}.Layout(gtx,
								layout.Stacked(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints.Min.X = gtx.Constraints.Max.X
									return u.pingGraph(gtx, results)
								}),
								layout.Stacked(func(gtx layout.Context) layout.Dimensions {
									if pinging {
										return layout.Dimensions{}
									}
									return u.pingBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										return chip(gtx, func(gtx layout.Context) layout.Dimensions {
											l := material.Body2(u.th, "Ping device")
											l.Color = colLine
											return l.Layout(gtx)
										})
									})
								}),
							)
						})
					}),
				)
			})
		})
	}

	// --- self-only sections ---
	if it.kind == kindSelf {
		gap(22)

		// Files.
		u.mu.Lock()
		files := u.files
		u.mu.Unlock()
		add(func(gtx layout.Context) layout.Dimensions { return u.sectionLabel(gtx, "Files", nil) })
		gap(8)
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
				if len(files) == 0 {
					return u.textRow(gtx, "No incoming files.")
				}
				var rows []layout.FlexChild
				for idx, f := range files {
					f, idx := f, idx
					if idx > 0 {
						rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
					}
					rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.fileRow(gtx, f, idx) }))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
			})
		})
		gap(22)

		// Advertised Routes.
		routes := u.advertisedRoutes()
		add(func(gtx layout.Context) layout.Dimensions {
			return u.sectionLabel(gtx, "Advertised Routes", &u.routeShowBtn)
		})
		gap(8)
		if u.showRouteBox {
			add(u.layoutRouteInput)
			gap(8)
		}
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
				if len(routes) == 0 {
					return u.textRow(gtx, "No advertised routes.")
				}
				var rows []layout.FlexChild
				for idx, r := range routes {
					r, idx := r, idx
					if idx > 0 {
						rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
					}
					rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.routeRow(gtx, r, idx) }))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
			})
		})
		gap(22)

		// Network Check.
		u.mu.Lock()
		nr := u.net
		u.mu.Unlock()
		add(func(gtx layout.Context) layout.Dimensions {
			return u.sectionLabel(gtx, "Network Check", &u.netCheckBtn)
		})
		gap(8)
		add(func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, func(gtx layout.Context) layout.Dimensions { return u.netCheckBody(gtx, nr) })
		})
	} // end self-only sections

	// Details (OS, key expiry, created, last seen).
	gap(22)
	add(func(gtx layout.Context) layout.Dimensions { return u.sectionLabel(gtx, "Details", nil) })
	gap(8)
	add(func(gtx layout.Context) layout.Dimensions {
		return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
			var rows []layout.FlexChild
			addInfo := func(k, v string) {
				if v == "" {
					return
				}
				if len(rows) > 0 {
					rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
				}
				rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.infoRow(gtx, k, v) }))
			}
			addInfo("OS", it.osName)
			addInfo("Key expiry", it.keyExpiry)
			addInfo("Created", it.created)
			addInfo("Last seen", it.lastSeen)
			if len(rows) == 0 {
				return u.textRow(gtx, "No details available.")
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
		})
	})

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// pingGraph draws a macOS-style latency graph: an auto-scaled Y axis with
// gridline labels on the right and a smoothed (Catmull-Rom) latency curve.
func (u *ui) pingGraph(gtx layout.Context, results []float64) layout.Dimensions {
	h := gtx.Dp(130)
	totalW := gtx.Constraints.Max.X
	labelW := gtx.Dp(52)
	graphW := totalW - labelW
	if graphW < gtx.Dp(40) {
		graphW, labelW = totalW, 0
	}

	// Auto-scale the Y axis to a "nice" maximum.
	maxV := 0.0
	for _, v := range results {
		if v > maxV {
			maxV = v
		}
	}
	scale := niceMax(maxV)

	fracs := []float32{0, 0.5, 1}
	for _, f := range fracs {
		y := int(float32(h-1) * f)
		paint.FillShape(gtx.Ops, colBorder, clip.Rect{Min: image.Pt(0, y), Max: image.Pt(graphW, y+1)}.Op())
	}
	if labelW > 0 {
		th16 := gtx.Dp(16)
		vals := []float64{scale, scale / 2, 0}
		ys := []int{0, h/2 - th16/2, h - th16}
		for i := range fracs {
			off := op.Offset(image.Pt(graphW+gtx.Dp(8), ys[i])).Push(gtx.Ops)
			gl := gtx
			gl.Constraints = layout.Constraints{Max: image.Pt(labelW, th16)}
			l := material.Caption(u.th, fmt.Sprintf("%dms", int(vals[i])))
			l.Color = colSub
			l.Layout(gl)
			off.Pop()
		}
	}

	if len(results) >= 2 {
		n := len(results)
		pt := func(i int) f32.Point {
			cv := results[i]
			if cv > scale {
				cv = scale
			}
			if cv < 0 {
				cv = 0
			}
			x := float32(i) / float32(n-1) * float32(graphW)
			y := float32(h) * (1 - float32(cv/scale))
			return f32.Pt(x, y)
		}
		var path clip.Path
		path.Begin(gtx.Ops)
		path.MoveTo(pt(0))
		for i := 0; i < n-1; i++ {
			p0, p1, p2, p3 := pt(clampi(i-1, 0, n-1)), pt(i), pt(i+1), pt(clampi(i+2, 0, n-1))
			c1 := f32.Pt(p1.X+(p2.X-p0.X)/6, p1.Y+(p2.Y-p0.Y)/6)
			c2 := f32.Pt(p2.X-(p3.X-p1.X)/6, p2.Y-(p3.Y-p1.Y)/6)
			path.CubeTo(c1, c2, p2)
		}
		paint.FillShape(gtx.Ops, colLine, clip.Stroke{Path: path.End(), Width: float32(gtx.Dp(2))}.Op())
	}
	return layout.Dimensions{Size: image.Pt(totalW, h)}
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 10
	}
	for _, s := range []float64{10, 25, 50, 100, 150, 250, 500, 750, 1000, 2000} {
		if v <= s {
			return s
		}
	}
	return math.Ceil(v/1000) * 1000
}

func clampi(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (u *ui) netCheckBody(gtx layout.Context, nr *netResult) layout.Dimensions {
	last := "Never"
	if u.netRunning {
		last = "Running…"
	} else if nr != nil {
		last = nr.when.Format("15:04:05")
	}
	rows := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.infoRow(gtx, "Last run", last) }),
	}
	if nr != nil {
		add := func(k, v string) {
			rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return divider(gtx, true) }))
			rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.infoRow(gtx, k, v) }))
		}
		add("UDP", yesno(nr.udp))
		add("IPv4", orDash(nr.ipv4))
		add("IPv6", orDash(nr.ipv6))
		if nr.derp != "" {
			add("Preferred DERP", nr.derp)
		}
		for _, l := range nr.lats {
			add("  "+l.name, l.dur.Round(time.Millisecond).String())
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

func (u *ui) layoutRouteInput(gtx layout.Context) layout.Dimensions {
	return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: 8, Bottom: 8, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					ed := material.Editor(u.th, &u.routeEditor, "10.0.0.0/24")
					ed.TextSize = unit.Sp(14)
					return ed.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.routeAddBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Body2(u.th, "Add")
						l.Color = colPink
						return layout.UniformInset(6).Layout(gtx, l.Layout)
					})
				}),
			)
		})
	})
}

// handleDetailInputs processes clicks/toggles for the current detail page.
func (u *ui) handleDetailInputs(gtx layout.Context, it *sbItem) {
	if it.kind == kindSelf {
		if u.optAdvertise.Update(gtx) {
			v := u.optAdvertise.Value
			u.do(func(ctx context.Context) error { return tsutil.AdvertiseExitNode(ctx, v) })
		}
		if u.optAllowLAN.Update(gtx) {
			v := u.optAllowLAN.Value
			u.do(func(ctx context.Context) error { return tsutil.AllowLANAccess(ctx, v) })
		}
		if u.optAcceptRoutes.Update(gtx) {
			v := u.optAcceptRoutes.Value
			u.do(func(ctx context.Context) error { return tsutil.AcceptRoutes(ctx, v) })
		}
		if u.optAcceptDNS.Update(gtx) {
			v := u.optAcceptDNS.Value
			u.do(func(ctx context.Context) error { return tsutil.AcceptDNS(ctx, v) })
		}
		if u.routeShowBtn.Clicked(gtx) {
			u.showRouteBox = !u.showRouteBox
		}
		if u.routeAddBtn.Clicked(gtx) {
			u.addRoute()
		}
		if u.netCheckBtn.Clicked(gtx) {
			u.runNetCheck()
		}
		for i := range u.routeRmBtns {
			if u.routeRmBtns[i].Clicked(gtx) {
				u.removeRoute(i)
			}
		}
	}
	if it.kind == kindPeer && it.exitOption {
		if u.peerExit.Update(gtx) {
			on := u.peerExit.Value
			id := it.id
			u.do(func(ctx context.Context) error {
				if on {
					return tsutil.ExitNode(ctx, id)
				}
				return tsutil.ExitNode(ctx, "")
			})
		}
	}
	if it.kind == kindPeer && it.fileTarget && u.sendFileBtn.Clicked(gtx) {
		id := it.id
		go u.sendFile(id)
	}
	if it.kind == kindPeer && u.pingBtn.Clicked(gtx) {
		u.mu.Lock()
		running := u.pinging && u.pingKey == it.key
		u.mu.Unlock()
		if running {
			u.stopPing()
		} else if ip, err := netip.ParseAddr(primaryIP(it.ips)); err == nil {
			slog.Info("ping start", "peer", it.title, "ip", ip.String())
			u.startPing(it.key, ip, it.online)
		} else {
			slog.Error("ping: bad peer IP", "ips", it.ips, "err", err)
		}
	}
}

// friendlyPingErr turns a raw LocalAPI ping error into a short, human message.
func friendlyPingErr(err error, online bool) string {
	s := err.Error()
	if strings.Contains(s, "deadline exceeded") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "timed out") || strings.Contains(s, "no response") {
		if !online {
			return "No response — device appears offline"
		}
		return "No response (timed out)"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 { // strip the verbose Post "url": prefix
		s = s[i+2:]
	}
	return s
}

// startPing (re)starts a continuous ping session for a peer. Any previous
// session is cancelled first. Uses a generation counter so only the newest
// loop is allowed to update UI state.
func (u *ui) startPing(key string, ip netip.Addr, online bool) {
	u.mu.Lock()
	if u.pingCancel != nil {
		u.pingCancel()
	}
	ctx, cancel := context.WithCancel(u.ctx)
	u.pingGen++
	gen := u.pingGen
	u.pingCancel = cancel
	u.pinging = true
	u.pingKey = key
	u.pingResults = nil
	u.pingConn = ""
	u.pingErr = ""
	u.mu.Unlock()
	go u.pingLoop(ctx, gen, ip, online)
}

// stopPing halts the current ping session.
func (u *ui) stopPing() {
	u.mu.Lock()
	if u.pingCancel != nil {
		u.pingCancel()
		u.pingCancel = nil
	}
	u.pingGen++ // invalidate any in-flight loop
	u.pinging = false
	u.mu.Unlock()
	u.w.Invalidate()
}

// pingLoop pings until its context is cancelled. It keeps retrying through
// failures (self-healing) and backs off after each failed attempt so it never
// piles up hung requests against an unreachable peer.
func (u *ui) pingLoop(ctx context.Context, gen int, ip netip.Addr, online bool) {
	defer func() {
		u.mu.Lock()
		if u.pingGen == gen {
			u.pinging = false
		}
		u.mu.Unlock()
		u.w.Invalidate()
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		rep, err := tsutil.PingOnce(pctx, ip)
		pcancel()
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		u.mu.Lock()
		if u.pingGen != gen {
			u.mu.Unlock()
			return
		}
		if err != nil {
			u.pingErr = friendlyPingErr(err, online)
			slog.Warn("ping failed", "ip", ip.String(), "err", err)
			wait = 1500 * time.Millisecond
		} else {
			u.pingErr = ""
			u.pingResults = append(u.pingResults, rep.LatencyMs)
			if len(u.pingResults) > 60 {
				u.pingResults = u.pingResults[len(u.pingResults)-60:]
			}
			switch {
			case rep.Direct:
				u.pingConn, u.pingDirect = "Direct connection", true
			case rep.Relay != "":
				u.pingConn, u.pingDirect = "DERP-relayed connection", false
			}
			slog.Debug("pong", "ip", ip.String(), "ms", rep.LatencyMs, "direct", rep.Direct, "relay", rep.Relay)
			wait = 600 * time.Millisecond
		}
		u.mu.Unlock()
		u.w.Invalidate()

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// sendFile prompts for a file and pushes it to the target via Taildrop.
func (u *ui) sendFile(target tailcfg.StableNodeID) {
	path, ok := openFileDialog()
	if !ok || path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Error("open file", "err", err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		slog.Error("stat file", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(u.ctx, 10*time.Minute)
	defer cancel()
	if err := tsutil.PushFile(ctx, target, st.Size(), filepath.Base(path), f); err != nil {
		slog.Error("taildrop send", "err", err)
	}
}

// --- model building -------------------------------------------------------

func (u *ui) rebuildModel(s *tsutil.IPNStatus) {
	items := make([]sbItem, 0, len(s.Peers)+8)

	// ownerLabel resolves a user's display/login name for a group header.
	ownerLabel := func(uid tailcfg.UserID) string {
		if s.NetMap != nil {
			if p, ok := s.NetMap.UserProfiles[uid]; ok && p.Valid() {
				if dn := p.DisplayName(); dn != "" {
					return dn
				}
				if ln := p.LoginName(); ln != "" {
					return ln
				}
			}
		}
		return "Unknown"
	}

	profile := ""
	var myUID tailcfg.UserID
	var selfItem *sbItem
	if s.NetMap != nil {
		myUID = s.NetMap.User()
		if p, ok := s.NetMap.UserProfiles[myUID]; ok && p.Valid() {
			profile = p.LoginName()
		}
		if profile == "" {
			profile = s.NetMap.Domain
		}
		self := s.NetMap.SelfNode
		selfDNS := strings.TrimSuffix(self.Name(), ".")
		selfIPs := nodeIPs(self)
		selfOS, selfKE, selfCreated, selfLastSeen := nodeDetail(self)
		si := sbItem{
			key: keySelf, kind: kindSelf, name: self.DisplayName(true), sub: "This machine",
			title: shortName(self), dnsName: selfDNS,
			ips: selfIPs, addrs: buildAddrs(selfDNS, selfIPs), online: s.Online(),
			avatar: colBlue, glyph: initial(self.DisplayName(true)),
			osName: selfOS, keyExpiry: selfKE, created: selfCreated, lastSeen: selfLastSeen,
		}
		selfItem = &si
	}

	hasMullvad := false
	for _, peer := range s.Peers {
		if tsutil.IsMullvad(peer) {
			hasMullvad = true
			break
		}
	}
	if hasMullvad {
		items = append(items, sbItem{key: keyMullvad, kind: kindMullvad, name: "Mullvad Exit Nodes",
			title: "Mullvad Exit Nodes", avatar: colYellow, glyph: "M"})
	}

	// Group devices by owner (login name), like the macOS client: the current
	// user's group (containing "this machine") comes first.
	currentExit := s.ExitNode()
	type group struct {
		label string
		mine  bool
		peers []sbItem
	}
	groups := map[tailcfg.UserID]*group{}
	var order []tailcfg.UserID
	ensureGroup := func(oid tailcfg.UserID) *group {
		g := groups[oid]
		if g == nil {
			g = &group{label: ownerLabel(oid), mine: oid == myUID}
			groups[oid] = g
			order = append(order, oid)
		}
		return g
	}
	if selfItem != nil {
		ensureGroup(myUID) // self's group exists even if it owns no peers
	}
	for id, peer := range s.Peers {
		if tsutil.IsMullvad(peer) {
			continue
		}
		g := ensureGroup(peer.User())
		online := peer.Online().Get()
		exitOption := tsaddr.ContainsExitRoutes(peer.AllowedIPs())
		av := colRed
		if online {
			av = colGreen
		}
		peerDNS := strings.TrimSuffix(peer.Name(), ".")
		peerIPs := nodeIPs(peer)
		pOS, pKE, pCreated, pLastSeen := nodeDetail(peer)
		g.peers = append(g.peers, sbItem{
			key: string(id), kind: kindPeer, id: id, name: peer.DisplayName(true), sub: primaryIP(peerIPs),
			title: shortName(peer), dnsName: peerDNS,
			ips: peerIPs, addrs: buildAddrs(peerDNS, peerIPs), online: online,
			avatar: av, glyph: initial(peer.DisplayName(true)),
			osName: pOS, keyExpiry: pKE, created: pCreated, lastSeen: pLastSeen,
			fileTarget: s.FileTargets.Contains(id),
			exitOption: exitOption,
			isExitNode: currentExit.Valid() && peer.StableID() == currentExit.StableID(),
		})
	}
	// Order groups: mine first, then alphabetically.
	sort.Slice(order, func(i, j int) bool {
		gi, gj := groups[order[i]], groups[order[j]]
		if gi.mine != gj.mine {
			return gi.mine
		}
		return strings.ToLower(gi.label) < strings.ToLower(gj.label)
	})
	for _, oid := range order {
		g := groups[oid]
		sort.Slice(g.peers, func(i, j int) bool { return strings.ToLower(g.peers[i].name) < strings.ToLower(g.peers[j].name) })
		online, total := 0, len(g.peers)
		for _, p := range g.peers {
			if p.online {
				online++
			}
		}
		if g.mine && selfItem != nil {
			total++
			if selfItem.online {
				online++
			}
		}
		items = append(items, sbItem{kind: kindHeader, title: g.label, hdrOnline: online, hdrTotal: total})
		if g.mine && selfItem != nil {
			items = append(items, *selfItem) // this machine first in its owner's group
		}
		items = append(items, g.peers...)
	}

	byKey := make(map[string]*sbItem, len(items))
	for i := range items {
		if items[i].kind != kindHeader {
			byKey[items[i].key] = &items[i]
		}
	}
	u.allItems = items
	u.byKey = byKey

	// Header info.
	u.mu.Lock()
	u.profile = profile
	if currentExit.Valid() {
		u.exitName = currentExit.DisplayName(true)
	} else {
		u.exitName = ""
	}
	u.mu.Unlock()
}

func (u *ui) syncFromStatus(s *tsutil.IPNStatus) {
	u.connect.Value = s.Online()
	if s.Prefs.Valid() {
		u.optAllowLAN.Value = s.Prefs.ExitNodeAllowLANAccess()
		u.exitAllowLAN.Value = s.Prefs.ExitNodeAllowLANAccess()
		u.optAcceptRoutes.Value = s.Prefs.RouteAll()
		u.optAcceptDNS.Value = s.Prefs.CorpDNS()
		u.optAdvertise.Value = tsaddr.ContainsExitRoutes(s.Prefs.AdvertiseRoutes())
	}
	u.syncedTo = ""
}

func (u *ui) advertisedRoutes() []netip.Prefix {
	u.mu.Lock()
	s := u.status
	u.mu.Unlock()
	if s == nil || !s.Prefs.Valid() {
		return nil
	}
	var out []netip.Prefix
	for _, r := range s.Prefs.AdvertiseRoutes().All() {
		if r.Bits() != 0 { // skip exit routes (0.0.0.0/0, ::/0)
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (u *ui) selected() *sbItem {
	if it, ok := u.byKey[u.selKey]; ok {
		return it
	}
	if it, ok := u.byKey[keySelf]; ok {
		return it
	}
	return nil
}

// --- actions --------------------------------------------------------------

func (u *ui) do(fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(u.ctx, 45*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			slog.Error("action failed", "err", err)
		}
	}()
}

func (u *ui) addRoute() {
	p, err := netip.ParsePrefix(strings.TrimSpace(u.routeEditor.Text()))
	if err != nil {
		slog.Error("parse prefix", "err", err)
		return
	}
	routes := append(u.advertisedRoutes(), p)
	u.routeEditor.SetText("")
	u.showRouteBox = false
	u.do(func(ctx context.Context) error { return tsutil.AdvertiseRoutes(ctx, routes) })
}

func (u *ui) removeRoute(i int) {
	routes := u.advertisedRoutes()
	if i < 0 || i >= len(routes) {
		return
	}
	next := append(routes[:i:i], routes[i+1:]...)
	u.do(func(ctx context.Context) error { return tsutil.AdvertiseRoutes(ctx, next) })
}

func (u *ui) runNetCheck() {
	u.mu.Lock()
	if u.netRunning {
		u.mu.Unlock()
		return
	}
	u.netRunning = true
	u.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(u.ctx, 30*time.Second)
		defer cancel()
		r, dm, err := tsutil.NetCheck(ctx, true)
		res := &netResult{when: time.Now()}
		if err != nil {
			slog.Error("netcheck", "err", err)
		} else {
			res.udp = r.UDP
			if r.IPv4 {
				res.ipv4 = r.GlobalV4.String()
			}
			if r.IPv6 {
				res.ipv6 = r.GlobalV6.String()
			}
			if dm != nil {
				if reg, ok := dm.Regions[r.PreferredDERP]; ok {
					res.derp = reg.RegionName
				}
				for id, lat := range r.RegionLatency {
					name := ""
					if reg, ok := dm.Regions[id]; ok {
						name = reg.RegionName
					}
					res.lats = append(res.lats, latEntry{name: name, dur: lat})
				}
				sort.Slice(res.lats, func(i, j int) bool { return res.lats[i].dur < res.lats[j].dur })
			}
		}
		u.mu.Lock()
		u.net = res
		u.netRunning = false
		u.mu.Unlock()
		u.w.Invalidate()
	}()
}

// --- small widgets --------------------------------------------------------

// statusDot draws a small macOS-style presence dot: green when online, grey
// when offline.
func (u *ui) statusDot(gtx layout.Context, online bool) layout.Dimensions {
	sz := gtx.Dp(10)
	col := colOfflineDot
	if online {
		col = colGreen
	}
	defer clip.Ellipse{Max: image.Pt(sz, sz)}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: col}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	return layout.Dimensions{Size: image.Pt(sz, sz)}
}

func (u *ui) avatar(gtx layout.Context, bg color.NRGBA, glyph string) layout.Dimensions {
	sz := gtx.Dp(34)
	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			defer clip.Ellipse{Max: image.Pt(sz, sz)}.Push(gtx.Ops).Pop()
			paint.ColorOp{Color: bg}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
			return layout.Dimensions{Size: image.Pt(sz, sz)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(15), glyph)
			l.Color = colWhite
			l.Font.Weight = font.Bold
			return layout.UniformInset(6).Layout(gtx, l.Layout)
		}),
	)
}

// sectionLabel draws a bold section title, optionally with a trailing round
// action button (used for the "+" route toggle and netcheck refresh).
func (u *ui) sectionLabel(gtx layout.Context, s string, btn *widget.Clickable) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			l := material.Body1(u.th, s)
			l.Font.Weight = font.Bold
			return l.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if btn == nil {
				return layout.Dimensions{}
			}
			glyph := "+"
			if btn == &u.netCheckBtn {
				glyph = "↻" // ↻
			}
			return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(u.th, glyph)
				l.Color = colPink
				l.Font.Weight = font.Bold
				return layout.UniformInset(4).Layout(gtx, l.Layout)
			})
		}),
	)
}

func (u *ui) card(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return widget.Border{Color: colBorder, CornerRadius: 12, Width: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				fillRRect(gtx, gtx.Constraints.Min, gtx.Dp(12), colCard)
				return layout.Dimensions{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return w(gtx)
			}),
		)
	})
}

// chip draws a light rounded, bordered container (for the search box and header
// exit-node selector).
func chip(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return widget.Border{Color: colBorder, CornerRadius: 8, Width: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				fillRRect(gtx, gtx.Constraints.Min, gtx.Dp(8), colWhite)
				return layout.Dimensions{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(8).Layout(gtx, w)
			}),
		)
	})
}

func (u *ui) switchRow(gtx layout.Context, label, subtitle string, b *widget.Bool) layout.Dimensions {
	return layout.Inset{Top: 10, Bottom: 10, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(material.Body1(u.th, label).Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if subtitle == "" {
							return layout.Dimensions{}
						}
						l := material.Caption(u.th, subtitle)
						l.Color = colSub
						return l.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Width: 10}.Layout),
			layout.Rigid(material.Switch(u.th, b, label).Layout),
		)
	})
}

func (u *ui) textRow(gtx layout.Context, s string) layout.Dimensions {
	return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		l := material.Body1(u.th, s)
		l.Color = colSub
		return l.Layout(gtx)
	})
}

func (u *ui) infoRow(gtx layout.Context, k, v string) layout.Dimensions {
	return layout.Inset{Top: 10, Bottom: 10, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, material.Body1(u.th, k).Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(u.th, v)
				l.Color = colSub
				l.MaxLines = 1
				return l.Layout(gtx)
			}),
		)
	})
}

func (u *ui) ipRow(gtx layout.Context, ip string, idx int) layout.Dimensions {
	for len(u.copyBtns) <= idx {
		u.copyBtns = append(u.copyBtns, widget.Clickable{})
	}
	btn := &u.copyBtns[idx]
	if btn.Clicked(gtx) {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(ip))})
	}
	return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(u.th, ip)
				l.MaxLines = 1
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, "Copy")
					l.Color = colPink
					return layout.UniformInset(4).Layout(gtx, l.Layout)
				})
			}),
		)
	})
}

func (u *ui) addrRow(gtx layout.Context, a addrEntry, idx int) layout.Dimensions {
	for len(u.copyBtns) <= idx {
		u.copyBtns = append(u.copyBtns, widget.Clickable{})
	}
	btn := &u.copyBtns[idx]
	if btn.Clicked(gtx) {
		val := a.value
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(val))})
	}
	return layout.Inset{Top: 10, Bottom: 10, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(u.th, a.value)
						l.MaxLines = 1
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Caption(u.th, a.label)
						l.Color = colSub
						return l.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, "Copy")
					l.Color = colPink
					return layout.UniformInset(4).Layout(gtx, l.Layout)
				})
			}),
		)
	})
}

func (u *ui) routeRow(gtx layout.Context, r netip.Prefix, idx int) layout.Dimensions {
	for len(u.routeRmBtns) <= idx {
		u.routeRmBtns = append(u.routeRmBtns, widget.Clickable{})
	}
	btn := &u.routeRmBtns[idx]
	return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(u.th, r.String())
				l.MaxLines = 1
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, "Remove")
					l.Color = colPink
					return layout.UniformInset(4).Layout(gtx, l.Layout)
				})
			}),
		)
	})
}

func (u *ui) fileRow(gtx layout.Context, f apitype.WaitingFile, idx int) layout.Dimensions {
	for len(u.fileDelBtns) <= idx {
		u.fileDelBtns = append(u.fileDelBtns, widget.Clickable{})
	}
	btn := &u.fileDelBtns[idx]
	if btn.Clicked(gtx) {
		name := f.Name
		u.do(func(ctx context.Context) error { return tsutil.DeleteWaitingFile(ctx, name) })
	}
	return layout.Inset{Top: 12, Bottom: 12, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(u.th, f.Name)
						l.MaxLines = 1
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Caption(u.th, humanBytes(f.Size))
						l.Color = colSub
						return l.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Body2(u.th, "Delete")
					l.Color = colPink
					return layout.UniformInset(4).Layout(gtx, l.Layout)
				})
			}),
		)
	})
}

func (u *ui) centerMsg(gtx layout.Context, s string) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		l := material.Body1(u.th, s)
		l.Color = colSub
		return l.Layout(gtx)
	})
}

func fillRRect(gtx layout.Context, size image.Point, radius int, col color.NRGBA) {
	rect := image.Rectangle{Max: size}
	defer clip.RRect{Rect: rect, SE: radius, SW: radius, NE: radius, NW: radius}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: col}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

func divider(gtx layout.Context, horizontal bool) layout.Dimensions {
	if horizontal {
		h := gtx.Dp(1)
		sz := image.Pt(gtx.Constraints.Max.X, h)
		paint.FillShape(gtx.Ops, colBorder, clip.Rect{Max: sz}.Op())
		return layout.Dimensions{Size: sz}
	}
	wpx := gtx.Dp(1)
	sz := image.Pt(wpx, gtx.Constraints.Max.Y)
	paint.FillShape(gtx.Ops, colBorder, clip.Rect{Max: sz}.Op())
	return layout.Dimensions{Size: sz}
}

func nodeIPs(n tailcfg.NodeView) []string {
	addrs := n.Addresses()
	out := make([]string, 0, addrs.Len())
	for i := range addrs.Len() {
		out = append(out, addrs.At(i).Addr().String())
	}
	return out
}

func primaryIP(ips []string) string {
	for _, ip := range ips {
		if strings.Contains(ip, ".") {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// buildAddrs produces the macOS-style "Tailscale addresses" rows: MagicDNS name
// first, then each IP tagged IPv4/IPv6.
func buildAddrs(dnsName string, ips []string) []addrEntry {
	var out []addrEntry
	if dnsName != "" {
		out = append(out, addrEntry{value: dnsName, label: "MagicDNS"})
	}
	for _, ip := range ips {
		label := "IPv6"
		if strings.Contains(ip, ".") {
			label = "IPv4"
		}
		out = append(out, addrEntry{value: ip, label: label})
	}
	return out
}

func nodeDetail(n tailcfg.NodeView) (osName, keyExpiry, created, lastSeen string) {
	if hi := n.Hostinfo(); hi.Valid() {
		osName = hi.OS()
	}
	if ke := n.KeyExpiry(); ke.IsZero() {
		keyExpiry = "Never"
	} else {
		keyExpiry = ke.Format("2006-01-02 15:04")
	}
	if c := n.Created(); !c.IsZero() {
		created = c.Format("2006-01-02 15:04")
	}
	if ls, ok := n.LastSeen().GetOk(); ok && !ls.IsZero() {
		lastSeen = ls.Format("2006-01-02 15:04")
	}
	return osName, keyExpiry, created, lastSeen
}

func shortName(n tailcfg.NodeView) string {
	name := strings.TrimSuffix(n.Name(), ".")
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	if name != "" {
		return name
	}
	return n.DisplayName(false)
}

func initial(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "?"
	}
	return strings.ToUpper(s[:1])
}

func yesno(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
