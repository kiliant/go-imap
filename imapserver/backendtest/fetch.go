package backendtest

import (
	"strings"

	"github.com/kiliant/go-imap"
)

func fetchValues(data *imap.FetchMessageData, key string) []imap.FetchData {
	if data == nil {
		return nil
	}
	var result []imap.FetchData
	for spelling, values := range data.Items {
		if strings.EqualFold(string(spelling), key) {
			result = append(result, values...)
		}
	}
	return result
}

func fetchValue[T imap.FetchData](data *imap.FetchMessageData, key string) (T, bool) {
	values := fetchValues(data, key)
	if len(values) == 1 {
		value, ok := values[0].(T)
		return value, ok
	}
	var zero T
	return zero, false
}
