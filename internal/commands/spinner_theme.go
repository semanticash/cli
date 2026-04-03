package commands

import (
	"io"

	hspinner "charm.land/huh/v2/spinner"
	"charm.land/lipgloss/v2"
)

func commandSpinner(title string, action func()) *hspinner.Spinner {
	return hspinner.New().
		WithTheme(commandSpinnerTheme()).
		Title(title).
		Action(action)
}

func commandSpinnerTheme() hspinner.Theme {
	return hspinner.ThemeFunc(func(isDark bool) *hspinner.Styles {
		styles := hspinner.ThemeDefault(isDark)
		green := lipgloss.LightDark(isDark)(
			lipgloss.Color("#02BA84"),
			lipgloss.Color("#02BF87"),
		)

		styles.Spinner = styles.Spinner.Foreground(green)
		return styles
	})
}

func runWithOptionalSpinner(out io.Writer, asJSON bool, title string, action func()) error {
	if asJSON || !isTerminal() || !isTerminalWriter(out) {
		action()
		return nil
	}
	return commandSpinner(title, action).Run()
}
