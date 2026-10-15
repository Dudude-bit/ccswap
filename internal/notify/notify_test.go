package notify

import "testing"

func TestOsascriptEscapes(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"Switched to b — café", `display notification "Switched to b — café" with title "ccswap"`},
		{`a" & (do shell script "rm") & "`, `display notification "a\" & (do shell script \"rm\") & \"" with title "ccswap"`},
		{`back\" slash`, `display notification "back\\\" slash" with title "ccswap"`},
		{"two\nlines\r", `display notification "two\nlines\r" with title "ccswap"`},
	} {
		if got := script("ccswap", tc.msg); got != tc.want {
			t.Errorf("script(%q)\n got %s\nwant %s", tc.msg, got, tc.want)
		}
	}
}

func TestRecorder(t *testing.T) {
	var r Recorder
	var n Notifier = &r
	n.Notify("ccswap", "x")
	if m := r.Messages(); len(m) != 1 || m[0] != "x" {
		t.Fatal(m)
	}
}

func TestNoneNotifier(t *testing.T) {
	var n Notifier = None{}
	n.Notify("ccswap", "x")
}
