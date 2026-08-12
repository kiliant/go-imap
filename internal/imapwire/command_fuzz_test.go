package imapwire

import "testing"

func FuzzBeginCommand(f *testing.F) {
	for _, seed := range []string{
		"A1 NOOP\r\n",
		"A2 SELECT inbox\r\n",
		"A3 APPEND inbox {3+}\r\nabc\r\n",
		"* invalid\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		dec := NewDecoderString(input, &Options{
			MaxLiteralSize:         1 << 20,
			MaxBufferedLiteralSize: 1 << 20,
			MaxLineLength:          8 << 10,
			MaxListDepth:           64,
		})
		_, _, err := dec.BeginCommand()
		if err != nil {
			_ = dec.DiscardLine()
			return
		}
		_ = dec.DiscardLine()
	})
}
