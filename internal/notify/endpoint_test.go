package notify

import (
	"strings"
	"testing"
)

func TestValidEndpoint(t *testing.T) {
	good := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/QAbc",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
		"https://FCM.GOOGLEAPIS.COM/fcm/send/abc",
	}
	bad := []string{
		"", "http://fcm.googleapis.com/x", "https://fcm.googleapis.com.evil.com/x", "https://evilfcm.googleapis.com.io/x",
		"https://push.apple.com.attacker.net/x", "https://127.0.0.1/x", "https://[::1]/x", "https://localhost/x",
		"https://169.254.169.254/x", "https://user@fcm.googleapis.com/x", "https://fcm.googleapis.com:444/x",
		"ftp://fcm.googleapis.com/x", "//fcm.googleapis.com/x", "javascript:alert(1)", "https://", "not a url",
	}
	for _, u := range good {
		if err := validEndpoint(u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validEndpoint(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if validEndpoint("https://fcm.googleapis.com/"+strings.Repeat("a", 3000)) == nil {
		t.Error("oversized endpoint accepted")
	}
}
