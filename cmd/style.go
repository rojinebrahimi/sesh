package cmd

import "github.com/charmbracelet/lipgloss"

var (
	sBold = lipgloss.NewStyle().Bold(true)
	sDim  = lipgloss.NewStyle().Faint(true)
	sOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	sWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	sErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	sAcc  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	sRole = map[string]lipgloss.Style{
		"user":      lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true),
		"assistant": lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true),
		"tool":      lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true),
		"system":    lipgloss.NewStyle().Faint(true).Bold(true),
	}
)

func pad(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	out := s
	for i := len(r); i < n; i++ {
		out += " "
	}
	return out
}
