package imapcodec

import (
	"strings"

	"github.com/kiliant/go-imap"
)

// FetchValues collects all spellings of one case-insensitive FETCH item.
func FetchValues(data *imap.FetchMessageData, key imap.FetchDataKey) []imap.FetchData {
	if data == nil {
		return nil
	}
	var result []imap.FetchData
	for spelling, values := range data.Items {
		if strings.EqualFold(string(spelling), string(key)) {
			result = append(result, values...)
		}
	}
	return result
}

// FetchValue requires exactly one value, including across case variants.
func FetchValue[T imap.FetchData](data *imap.FetchMessageData, key imap.FetchDataKey) (T, bool) {
	values := FetchValues(data, key)
	if len(values) == 1 {
		value, ok := values[0].(T)
		return value, ok
	}
	var zero T
	return zero, false
}
