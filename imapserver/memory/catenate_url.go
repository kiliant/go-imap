package memory

import (
	"net/url"
	"strings"
	"time"
)

// Relative URLs inherit this authenticated session, and message-only URLs
// inherit its selected mailbox. Absolute URLs may name only the same account.
// Their host and port are logical aliases, as documented by package memory.
func parseCatenateURL(raw, username, selected string) (*urlAuthURL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" {
		return nil, false
	}
	if u.Scheme == "" && u.Host == "" {
		path := u.EscapedPath()
		if strings.HasPrefix(strings.ToUpper(path), ";UID=") || strings.HasPrefix(strings.ToUpper(path), ";UIDVALIDITY=") {
			if selected == "" {
				return nil, false
			}
			separator := "/"
			if strings.HasPrefix(strings.ToUpper(path), ";UIDVALIDITY=") {
				separator = ""
			}
			path = url.PathEscape(selected) + separator + path
		}
		raw = "imap://" + userinfoEscape(username) + "@localhost/" + strings.TrimPrefix(path, "/")
	} else {
		if !strings.EqualFold(u.Scheme, "imap") || u.Hostname() == "" {
			return nil, false
		}
		if u.User == nil {
			_, rest, _ := strings.Cut(raw, "://")
			raw = "imap://" + userinfoEscape(username) + "@" + rest
		}
	}
	request, ok := parseURLAuthURL(raw+";URLAUTH=authuser", false, time.Now())
	if !ok || request.owner != username {
		return nil, false
	}
	return request, true
}

// userinfoEscape writes a login as RFC 5092 enc-user. url.PathEscape keeps "@"
// and ":", which delimit userinfo, so a login that is an e-mail address would
// otherwise give the authority a second "@".
func userinfoEscape(username string) string {
	return strings.NewReplacer("@", "%40", ":", "%3A").Replace(url.PathEscape(username))
}
