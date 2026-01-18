package httpserver

func renderErrorPage(_ int, _ string, errorMsg string) []byte {
	return []byte(errorMsg)
}
