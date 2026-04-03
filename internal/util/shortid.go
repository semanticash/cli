package util

// ShortIDLen is the number of characters shown in human-facing checkpoint IDs.
const ShortIDLen = 8

// ShortID returns the first ShortIDLen characters of a full ID.
// If the input is shorter than ShortIDLen, it is returned unchanged.
func ShortID(full string) string {
	if len(full) <= ShortIDLen {
		return full
	}
	return full[:ShortIDLen]
}
