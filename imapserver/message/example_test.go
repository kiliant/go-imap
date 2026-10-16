package message_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

func ExampleAnalyze() {
	ctx := context.Background()
	raw := "Subject: Example\r\n\r\nHello\r\n"
	m, err := message.Analyze(ctx, strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		panic(err)
	}
	reader, size, err := m.OpenBodySection(ctx, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText})
	if err != nil {
		panic(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		panic(err)
	}
	fmt.Println(m.Envelope().Subject, size)
	fmt.Print(string(body))
	// Output:
	// Example 7
	// Hello
}
