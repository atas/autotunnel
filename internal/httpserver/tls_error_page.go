package httpserver

// renderErrorPage returns a plain text error message.
func renderErrorPage(_ int, _ string, errorMsg string) []byte {
	return []byte(errorMsg)
}
