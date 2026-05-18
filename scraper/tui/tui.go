// Package tui renders an interactive Bubbletea dashboard for an in-flight
// scrape. The centerpiece is a discovery tree showing the chain by which
// each page was reached (root → linked child → grandchild).
//
// Events flow scraper → tea.Program.Send → Model.Update. The parent
// process MUST silence/redirect any other stdout/stderr writers (logger,
// progressbar) to avoid corrupting the rendered frame.
package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cornelk/goscrape/scraper"
)

// inflightPair carries one in-flight asset URL with its start time. Used
// as a snapshot during render — not stored in the model.
type inflightPair struct {
	url     string
	started time.Time
}

const (
	// maxRecent caps the rolling notice log size.
	maxRecent = 6
	// maxRecentCompleted caps the rolling list of recently finished pages
	// shown below the active path. Keeps render bounded on big crawls.
	maxRecentCompleted = 8
	// maxInflightShown caps the rendered list of in-flight asset URLs.
	maxInflightShown = 10
	// activePathLevels is the depth window for the active path: only the
	// N deepest ancestors of the current page are shown. As crawl goes
	// deeper, the topmost ancestor scrolls off; as it comes back up, the
	// view slides up to show the root. Always exactly N (or fewer if
	// crawl depth < N) levels — the panel never grows.
	activePathLevels = 3
)

// DoneMsg is sent by the scraper runner once Scraper.Start has returned.
// Err is nil on clean completion.
type DoneMsg struct {
	Err error
}

// tickMsg drives the elapsed-time and spinner update.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// pageStatus is the lifecycle state of a discovered page in the crawl tree.
type pageStatus int

const (
	statusPending pageStatus = iota
	statusDownloading
	statusDone
	statusFailed
	statusSkipped
)

// pageNode is one node in the discovery tree.
type pageNode struct {
	url      string
	parent   string // "" for root
	depth    uint
	status   pageStatus
	err      string
	started  time.Time
	finished time.Time

	// Asset counters attributed to this page (assets discovered while
	// this page was current).
	assetsOK       int
	assetsFailed   int
	assetsSkipped  int
	assetsInflight int

	// children stores child URLs in discovery order so the tree renders
	// in a stable, meaningful sequence.
	children []string
}

type recentEntry struct {
	at      time.Time
	kind    scraper.EventKind
	url     string
	message string
	err     string
}

// completedEntry is a single entry in the recently-completed ring shown
// below the active path. We snapshot the salient fields at completion
// time so render doesn't need to re-look-up the (possibly evicted) node.
type completedEntry struct {
	url      string
	status   pageStatus
	duration time.Duration
	at       time.Time
	err      string
}

// Model is the Bubbletea model for the scraping dashboard.
type Model struct {
	target    string
	startedAt time.Time
	width     int
	height    int

	pagesOK      int
	pagesFailed  int
	assetsOK     int
	assetsFailed int
	skipped      int
	queueSize    int

	// Tree state. Maintained for lineage (parent chain) — NOT rendered
	// in full. Rendering only visits a small subset to keep frame cost
	// bounded on huge crawls.
	pages       map[string]*pageNode
	rootURL     string
	currentPage string // URL of the page currently being processed by scraper

	// inflightAssets tracks asset URLs currently being downloaded so we
	// can show them as a short live list. Keyed by URL → start time.
	inflightAssets map[string]time.Time

	// recentCompleted is a ring of URLs that just finished (done /
	// failed / skipped). Shows recent throughput without rendering the
	// whole tree.
	recentCompleted []completedEntry

	recent []recentEntry

	done    bool
	doneErr error
	doneAt  time.Time
}

// New constructs the initial model. target is the URL being scraped; it is
// shown in the header.
func New(target string) Model {
	return Model{
		target:         target,
		startedAt:      time.Now(),
		pages:          make(map[string]*pageNode),
		inflightAssets: make(map[string]time.Time),
	}
}

// Init starts the elapsed-time ticker.
func (m Model) Init() tea.Cmd { return tickCmd() }

// Update mutates the model in response to messages and returns the next
// command. The tea.Model return is dictated by bubbletea's interface.
//
//nolint:ireturn // required by tea.Model interface
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		if m.done {
			return m, nil
		}
		return m, tickCmd()

	case scraper.Event:
		m = m.applyEvent(msg)
		return m, nil

	case DoneMsg:
		m.done = true
		m.doneErr = msg.Err
		m.doneAt = time.Now()
		// Final repaint then auto-quit after a brief moment so the user
		// sees the final state before returning to the shell.
		return m, tea.Sequence(
			func() tea.Msg { return tickMsg(time.Now()) },
			tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return tea.QuitMsg{} }),
		)
	}
	return m, nil
}

// applyEvent folds a single scraper event into the model. Complexity
// scales with the event-kind switch; each branch is a coherent state
// transition.
//
//nolint:cyclop,funlen // event-state-machine dispatch
func (m Model) applyEvent(ev scraper.Event) Model {
	switch ev.Kind {
	case scraper.EventPageDiscovered:
		m.ensurePageNode(ev.URL, ev.Parent, ev.Depth)

	case scraper.EventPageStart:
		node := m.ensurePageNode(ev.URL, "", ev.Depth)
		node.status = statusDownloading
		node.started = time.Now()
		m.currentPage = ev.URL

	case scraper.EventPageDone:
		// When the scraper redirects the start URL, PageDone carries the
		// post-redirect URL while the EventPageDiscovered we registered
		// used the pre-redirect URL. Resolve to the active page so the
		// status update lands on the right node.
		target := ev.URL
		if _, ok := m.pages[target]; !ok {
			target = m.currentPage
		}
		if node := m.pages[target]; node != nil {
			node.status = statusDone
			node.finished = time.Now()
			m.pagesOK++
			m.recordCompleted(node)
		}
		if m.currentPage == ev.URL || m.currentPage == target {
			m.currentPage = ""
		}

	case scraper.EventAssetStart:
		m.inflightAssets[ev.URL] = time.Now()
		if cp := m.pages[m.currentPage]; cp != nil {
			cp.assetsInflight++
		}

	case scraper.EventAssetDone:
		delete(m.inflightAssets, ev.URL)
		if cp := m.pages[m.currentPage]; cp != nil {
			if cp.assetsInflight > 0 {
				cp.assetsInflight--
			}
			cp.assetsOK++
		}
		m.assetsOK++

	case scraper.EventSkipped:
		if node, ok := m.pages[ev.URL]; ok {
			node.status = statusSkipped
			node.finished = time.Now()
			m.skipped++
			m.recordCompleted(node)
		} else {
			delete(m.inflightAssets, ev.URL)
			if cp := m.pages[m.currentPage]; cp != nil {
				cp.assetsSkipped++
			}
			m.skipped++
		}
		m.recent = appendRecent(m.recent, recentEntry{
			at: ev.Timestamp, kind: ev.Kind, url: ev.URL, message: ev.Message,
		})

	case scraper.EventFailed:
		if node, ok := m.pages[ev.URL]; ok {
			node.status = statusFailed
			node.err = ev.Err
			node.finished = time.Now()
			m.pagesFailed++
			m.recordCompleted(node)
		} else {
			delete(m.inflightAssets, ev.URL)
			if cp := m.pages[m.currentPage]; cp != nil {
				if cp.assetsInflight > 0 {
					cp.assetsInflight--
				}
				cp.assetsFailed++
			}
			m.assetsFailed++
		}
		m.recent = appendRecent(m.recent, recentEntry{
			at: ev.Timestamp, kind: ev.Kind, url: ev.URL, err: ev.Err,
		})

	case scraper.EventQueueChanged:
		m.queueSize = ev.QueueSize

	case scraper.EventLog:
		m.recent = appendRecent(m.recent, recentEntry{
			at: ev.Timestamp, kind: ev.Kind, message: ev.Message,
		})
	}
	return m
}

// recordCompleted appends the just-finished page node to the recent ring
// buffer. The ring is bounded by maxRecentCompleted so total memory stays
// O(maxRecentCompleted) regardless of how many pages were crawled.
func (m *Model) recordCompleted(n *pageNode) {
	if n == nil {
		return
	}
	var dur time.Duration
	if !n.started.IsZero() && !n.finished.IsZero() {
		dur = n.finished.Sub(n.started)
	}
	entry := completedEntry{
		url:      n.url,
		status:   n.status,
		duration: dur,
		at:       n.finished,
		err:      n.err,
	}
	if len(m.recentCompleted) >= maxRecentCompleted {
		// Drop oldest entry in place to keep allocation bounded.
		m.recentCompleted = append(m.recentCompleted[:0], m.recentCompleted[1:]...)
	}
	m.recentCompleted = append(m.recentCompleted, entry)
}

// ensurePageNode returns the node for url, creating it if missing. If
// parent is non-empty and the node is new (or its parent slot was empty),
// it is linked under the parent's children list. The first node created
// is recorded as the tree root.
func (m *Model) ensurePageNode(url, parent string, depth uint) *pageNode {
	if existing, ok := m.pages[url]; ok {
		// Link to parent if we now have one and existing has none.
		if parent != "" && existing.parent == "" {
			existing.parent = parent
			if p := m.pages[parent]; p != nil && !containsString(p.children, url) {
				p.children = append(p.children, url)
			}
		}
		return existing
	}

	node := &pageNode{
		url:    url,
		parent: parent,
		depth:  depth,
		status: statusPending,
	}
	m.pages[url] = node

	if parent == "" {
		if m.rootURL == "" {
			m.rootURL = url
		}
	} else if p := m.pages[parent]; p != nil {
		p.children = append(p.children, url)
	}
	// If parent is named but unknown (e.g. start-page redirect — the
	// redirected URL was never announced as discovered), the child is
	// left as an orphan. It is reachable via m.pages but not via tree
	// walk from rootURL. Acceptable: scraper-side fix is to also emit
	// EventPageDiscovered for the redirected URL.
	return node
}

// ----- Styles -----

var (
	colorPrimary = lipgloss.Color("#7D56F4")
	colorSuccess = lipgloss.Color("#28A745")
	colorWarning = lipgloss.Color("#FFC107")
	colorDanger  = lipgloss.Color("#DC3545")
	colorInfo    = lipgloss.Color("#17A2B8")
	colorDim     = lipgloss.Color("#6C757D")
	colorMuted   = lipgloss.Color("#ADB5BD")
	colorBorder  = lipgloss.Color("#4A4A6A")

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(colorPrimary).
			Bold(true).
			Padding(0, 2)

	dimStyle     = lipgloss.NewStyle().Foreground(colorDim)
	mutedStyle   = lipgloss.NewStyle().Foreground(colorMuted)
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)

	successStyle = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	dangerStyle  = lipgloss.NewStyle().Foreground(colorDanger).Bold(true)
	warningStyle = lipgloss.NewStyle().Foreground(colorWarning).Bold(true)
	infoStyle    = lipgloss.NewStyle().Foreground(colorInfo).Bold(true)

	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#1E1E2E")).
			Foreground(colorMuted).
			Padding(0, 1)
)

// View renders the model.
func (m Model) View() string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	// No max-width cap — use the full terminal so long URLs in the tree
	// have room.

	var b strings.Builder
	b.WriteString(m.renderHeader(w))
	b.WriteString("\n\n")
	b.WriteString(m.renderStats(w))
	b.WriteString("\n\n")
	b.WriteString("  " + sectionStyle.Render("ACTIVE PATH") +
		"  " + dimStyle.Render("(root → current page; ancestors = how we got here)"))
	b.WriteString("\n")
	b.WriteString(m.renderActivePath())

	b.WriteString("\n  ")
	b.WriteString(sectionStyle.Render("IN-FLIGHT ASSETS"))
	b.WriteString("  " + dimStyle.Render(fmt.Sprintf("(%d)", len(m.inflightAssets))))
	b.WriteString("\n")
	b.WriteString(m.renderInflightAssets())

	b.WriteString("\n  ")
	b.WriteString(sectionStyle.Render("RECENTLY COMPLETED"))
	b.WriteString("  " + dimStyle.Render(fmt.Sprintf("(last %d)", len(m.recentCompleted))))
	b.WriteString("\n")
	b.WriteString(m.renderRecentCompleted())

	if len(m.recent) > 0 {
		b.WriteString("\n  ")
		b.WriteString(sectionStyle.Render("NOTICES"))
		b.WriteString("\n")
		b.WriteString(m.renderRecent())
	}
	b.WriteString("\n")
	b.WriteString(m.renderStatusBar(w))
	return b.String()
}

func (m Model) renderHeader(w int) string {
	state := "● SCRAPING"
	stateColor := colorPrimary
	if m.done {
		if m.doneErr != nil {
			state = "✗ FAILED"
			stateColor = colorDanger
		} else {
			state = "✓ DONE"
			stateColor = colorSuccess
		}
	}

	left := lipgloss.NewStyle().Foreground(stateColor).Bold(true).Render(state) +
		"  " + mutedStyle.Render(m.target)

	right := fmt.Sprintf("Elapsed %s  |  Q %d  |  Pages %d",
		fmtDuration(time.Since(m.startedAt)),
		m.queueSize,
		len(m.pages),
	)
	rightStyled := mutedStyle.Render(right)

	gap := w - lipgloss.Width(left) - lipgloss.Width(rightStyled) - 4
	if gap < 1 {
		gap = 1
	}
	bar := lipgloss.NewStyle().
		Background(lipgloss.Color("#2A2A3A")).
		Padding(0, 2).
		Width(w).
		Render(titleStyle.Render(" goscrape ") + " " + left + strings.Repeat(" ", gap) + rightStyled)
	return bar
}

func (m Model) renderStats(w int) string {
	cardInner := (w / 6) - 4
	if cardInner < 10 {
		cardInner = 10
	}

	mkCard := func(label, value string, color lipgloss.Color) string {
		v := lipgloss.NewStyle().Bold(true).Foreground(color).Render(value)
		return cardStyle.Width(cardInner).Render(
			mutedStyle.Render(label) + "\n" + v,
		)
	}

	pageFailedColor := colorDim
	if m.pagesFailed > 0 {
		pageFailedColor = colorDanger
	}
	assetFailedColor := colorDim
	if m.assetsFailed > 0 {
		assetFailedColor = colorDanger
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top,
		mkCard("PAGES OK", strconv.Itoa(m.pagesOK), colorSuccess),
		mkCard("PAGES FAIL", strconv.Itoa(m.pagesFailed), pageFailedColor),
		mkCard("ASSETS OK", strconv.Itoa(m.assetsOK), colorSuccess),
		mkCard("ASSETS FAIL", strconv.Itoa(m.assetsFailed), assetFailedColor),
		mkCard("SKIPPED", strconv.Itoa(m.skipped), colorWarning),
		mkCard("QUEUE", strconv.Itoa(m.queueSize), colorInfo),
	)
	return row
}

// renderActivePath renders the lineage chain leading to the current page
// as a single linear path, clamped to activePathLevels rows. As the crawl
// descends, the topmost ancestor scrolls off; as it ascends, the root
// slides back into view. Always ≤ activePathLevels rows — the panel
// never grows regardless of crawl depth.
func (m Model) renderActivePath() string {
	leaf := m.currentPage
	if leaf == "" && len(m.recentCompleted) > 0 {
		leaf = m.recentCompleted[len(m.recentCompleted)-1].url
	}
	if leaf == "" {
		leaf = m.rootURL
	}
	if leaf == "" {
		return "  " + dimStyle.Render("(no pages yet)") + "\n"
	}

	// Walk parent chain up to root. Bounded at 64 to defend against a
	// malformed parent cycle; --depth is typically ≤ 10 in practice.
	var path []string
	for cur := leaf; cur != "" && len(path) < 64; {
		path = append(path, cur)
		node := m.pages[cur]
		if node == nil {
			break
		}
		cur = node.parent
	}
	// Reverse to get root-first ordering.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	// Slide the window: keep only the deepest activePathLevels entries.
	if len(path) > activePathLevels {
		path = path[len(path)-activePathLevels:]
	}

	var b strings.Builder
	for i, url := range path {
		node := m.pages[url]
		if node == nil {
			continue
		}
		var connector, indent string
		if i == 0 {
			connector = ""
			indent = ""
		} else {
			indent = strings.Repeat("   ", i-1)
			connector = "└─ "
		}
		fmt.Fprintf(&b, "%s%s%s\n",
			indent,
			dimStyle.Render(connector),
			renderNodeLabel(node, url == m.currentPage),
		)
	}
	return b.String()
}

// renderInflightAssets shows asset URLs currently being downloaded. Bounded
// at maxInflightShown rows; remainder is summarized.
func (m Model) renderInflightAssets() string {
	if len(m.inflightAssets) == 0 {
		return "  " + dimStyle.Render("(no assets in flight)") + "\n"
	}

	// Snapshot URLs sorted by elapsed time (longest first) so the
	// stragglers are visible.
	pairs := make([]inflightPair, 0, len(m.inflightAssets))
	for u, t := range m.inflightAssets {
		pairs = append(pairs, inflightPair{url: u, started: t})
	}
	slices.SortFunc(pairs, func(a, b inflightPair) int {
		return cmp.Compare(a.started.UnixNano(), b.started.UnixNano())
	})

	var b strings.Builder
	limit := len(pairs)
	overflow := 0
	if limit > maxInflightShown {
		overflow = limit - maxInflightShown
		limit = maxInflightShown
	}
	for i := range limit {
		p := pairs[i]
		dur := fmtDuration(time.Since(p.started))
		fmt.Fprintf(&b, "  %s %s  %s\n",
			infoStyle.Render("◐"),
			mutedStyle.Render(fmt.Sprintf("%5s", dur)),
			p.url,
		)
	}
	if overflow > 0 {
		fmt.Fprintf(&b, "  %s\n",
			dimStyle.Render(fmt.Sprintf("… + %d more", overflow)))
	}
	return b.String()
}

// renderRecentCompleted prints the recent-completion ring buffer (newest
// first). Bounded by maxRecentCompleted entries.
func (m Model) renderRecentCompleted() string {
	if len(m.recentCompleted) == 0 {
		return "  " + dimStyle.Render("(none yet)") + "\n"
	}
	var b strings.Builder
	for i := len(m.recentCompleted) - 1; i >= 0; i-- {
		c := m.recentCompleted[i]
		icon, st := iconAndStyleForStatus(c.status)
		var detail string
		switch c.status {
		case statusFailed:
			detail = dangerStyle.Render(truncate(c.err, 60))
		case statusSkipped:
			detail = warningStyle.Render("skipped")
		case statusDone:
			detail = mutedStyle.Render(fmtDuration(c.duration))
		}
		fmt.Fprintf(&b, "  %s %s  %s\n", st.Render(icon), c.url, detail)
	}
	return b.String()
}

// renderNodeLabel formats a single page node into a colored, single-line
// label combining icon + URL + status detail + asset summary.
func renderNodeLabel(n *pageNode, isCurrent bool) string {
	icon, st := iconAndStyleForStatus(n.status)
	if isCurrent && n.status == statusDownloading {
		// Highlight currently active page brighter.
		st = infoStyle
	}

	label := st.Render(icon) + " " + n.url

	var tail string
	switch n.status {
	case statusDownloading:
		tail = "  " + mutedStyle.Render(fmtDuration(time.Since(n.started)))
	case statusDone:
		if !n.started.IsZero() && !n.finished.IsZero() {
			tail = "  " + mutedStyle.Render(fmtDuration(n.finished.Sub(n.started)))
		}
	case statusFailed:
		errMsg := n.err
		if errMsg == "" {
			errMsg = "failed"
		}
		tail = "  " + dangerStyle.Render(truncate(errMsg, 60))
	case statusSkipped:
		tail = "  " + warningStyle.Render("skipped")
	}

	asset := renderAssetSummary(n)
	if asset != "" {
		tail += "  " + asset
	}

	return label + tail
}

func renderAssetSummary(n *pageNode) string {
	total := n.assetsOK + n.assetsFailed + n.assetsSkipped + n.assetsInflight
	if total == 0 {
		return ""
	}
	var parts []string
	if n.assetsInflight > 0 {
		parts = append(parts, infoStyle.Render(fmt.Sprintf("%d◐", n.assetsInflight)))
	}
	if n.assetsOK > 0 {
		parts = append(parts, successStyle.Render(fmt.Sprintf("%d✓", n.assetsOK)))
	}
	if n.assetsFailed > 0 {
		parts = append(parts, dangerStyle.Render(fmt.Sprintf("%d✗", n.assetsFailed)))
	}
	if n.assetsSkipped > 0 {
		parts = append(parts, warningStyle.Render(fmt.Sprintf("%d⊘", n.assetsSkipped)))
	}
	return dimStyle.Render("assets:") + " " + strings.Join(parts, " ")
}

func iconAndStyleForStatus(s pageStatus) (string, lipgloss.Style) {
	switch s {
	case statusDownloading:
		return "◐", warningStyle
	case statusDone:
		return "✓", successStyle
	case statusFailed:
		return "✗", dangerStyle
	case statusSkipped:
		return "⊘", warningStyle
	default:
		return "○", dimStyle
	}
}

func (m Model) renderRecent() string {
	var b strings.Builder
	for i := len(m.recent) - 1; i >= 0; i-- {
		r := m.recent[i]
		ts := mutedStyle.Render(r.at.Format("15:04:05"))
		var tag, info string
		switch r.kind {
		case scraper.EventFailed:
			tag = dangerStyle.Render("FAIL  ")
			info = r.err
			if info == "" {
				info = "(unknown error)"
			}
		case scraper.EventSkipped:
			tag = warningStyle.Render("SKIP  ")
			info = r.message
		default:
			tag = mutedStyle.Render("EVENT ")
			info = r.message
		}
		urlPart := truncate(r.url, 70)
		fmt.Fprintf(&b, "  %s %s %s  %s\n",
			ts, tag, urlPart, dimStyle.Render(truncate(info, 80)))
	}
	return b.String()
}

func (m Model) renderStatusBar(w int) string {
	keys := []string{"[Q]uit"}
	if m.done {
		if m.doneErr != nil {
			keys = append([]string{dangerStyle.Render("✗ " + truncate(m.doneErr.Error(), 80))}, keys...)
		} else {
			keys = append([]string{successStyle.Render("✓ scrape complete")}, keys...)
		}
	}
	return statusBarStyle.Width(w).Render(strings.Join(keys, "    "))
}

// ----- Helpers -----

func appendRecent(recent []recentEntry, e recentEntry) []recentEntry {
	if len(recent) >= maxRecent {
		recent = append(recent[:0], recent[1:]...)
	}
	return append(recent, e)
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// truncate shortens s to fit within maxLen runes, adding an ellipsis.
// Operates on rune count, not bytes — Unicode-safe for non-ASCII URLs.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	return string(runes[:maxLen-1]) + "…"
}

// fmtDuration prints a duration as a compact h/m/s string.
func fmtDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// ----- Sender bridge -----

// Sender is a tiny wrapper that lets the scraper push events into the TUI
// program without taking a dependency on *tea.Program.
type Sender interface {
	Send(tea.Msg)
}

// NewEventHandler returns a scraper.EventHandler that forwards events to
// the given sender as tea.Msg values. Safe to call from any goroutine —
// tea.Program.Send is thread-safe.
//
// The returned stop function zeroes the sender, guarding against a TOCTOU
// race where the caller closes the program while scraper goroutines are
// still emitting.
func NewEventHandler(s Sender) (scraper.EventHandler, func()) {
	var mu sync.Mutex
	live := s
	stop := func() {
		mu.Lock()
		live = nil
		mu.Unlock()
	}
	handler := func(ev scraper.Event) {
		mu.Lock()
		target := live
		mu.Unlock()
		if target == nil {
			return
		}
		target.Send(ev)
	}
	return handler, stop
}
