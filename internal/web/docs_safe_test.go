package web

import (
	"strings"
	"testing"
)

// renderMDSafe must neutralise payloads that ride in on ingested note bodies: inline
// HTML (a raw <script>) is dropped and unsafe link schemes (javascript:) are not
// emitted as live hrefs, while legitimate prose survives. renderMD (trusted embedded
// docs) intentionally does NOT sanitise, so the two must differ on hostile input,
// proving the safe path is actually doing work.
func TestRenderMDSafeStripsHostileHTML(t *testing.T) {
	src := []byte("# Title\n\n<script>alert(1)</script>\n\n[click](javascript:alert(2))\n\nplain text\n")

	safe := renderMDSafe(src)
	if strings.Contains(safe, "<script>") {
		t.Errorf("renderMDSafe leaked a <script> tag:\n%s", safe)
	}
	if strings.Contains(strings.ToLower(safe), "javascript:") {
		t.Errorf("renderMDSafe emitted a javascript: link:\n%s", safe)
	}
	if !strings.Contains(safe, "plain text") {
		t.Errorf("renderMDSafe dropped legitimate content:\n%s", safe)
	}

	if !strings.Contains(renderMD(src), "<script>") {
		t.Error("renderMD unexpectedly stripped HTML; the trusted/untrusted split would be moot")
	}
}
