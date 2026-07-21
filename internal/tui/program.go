package tui

import tea "github.com/charmbracelet/bubbletea"

// Program wraps a Bubble Tea program so the host can run the incident console and
// feed it monitoring messages while reading operator actions from the channel.
//
// Typical wiring:
//
//	actions := make(chan Action, 8)
//	p := tui.NewProgram(tui.New(label, actions))
//	go func() { for a := range actions { dispatch(a) } }()  // host -> executor
//	go func() { for s := range samples { p.Send(tui.BlockersMsg{...}) } }() // monitor -> TUI
//	p.Run()
type Program struct {
	p *tea.Program
}

// NewProgram builds a Program around the given model. It runs on the alternate screen: the
// dashboard is a fixed multi-panel layout that repaints every second, so it takes over the
// terminal for a clean in-place redraw and restores the prior screen (with the post-run
// summary printed to stdout) on exit.
func NewProgram(m Model) *Program {
	return &Program{p: tea.NewProgram(m, tea.WithAltScreen())}
}

// Send delivers a message (e.g. ProgressMsg, BlockersMsg) to the running console.
func (pr *Program) Send(msg tea.Msg) { pr.p.Send(msg) }

// Run starts the console and blocks until the operator quits.
func (pr *Program) Run() error {
	_, err := pr.p.Run()
	return err
}

// Quit stops the console.
func (pr *Program) Quit() { pr.p.Quit() }
