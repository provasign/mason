package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

// tiny 1x1 PNG
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

func TestImagesMentionedAttachToTask(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shot.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "a screenshot"}}}
	s := New(fp, nil, Options{Root: root, Out: io.Discard})
	if _, err := s.Ask(context.Background(), "describe shot.png briefly"); err != nil {
		t.Fatal(err)
	}
	var taskMsg *provider.Msg
	for i := range s.msgs {
		if s.msgs[i].Role == "user" && strings.Contains(s.msgs[i].Content, "describe") {
			taskMsg = &s.msgs[i]
		}
	}
	if taskMsg == nil || len(taskMsg.Images) != 1 {
		t.Fatalf("image must be attached to the task message: %+v", taskMsg)
	}
	if taskMsg.Images[0].MediaType != "image/png" || taskMsg.Images[0].DataB64 == "" {
		t.Fatalf("attachment wrong: %+v", taskMsg.Images[0])
	}
	// The binary must NOT also be text-attached by the file pre-seeder.
	for _, m := range s.msgs {
		if strings.Contains(m.Content, "[mason attached shot.png") {
			t.Fatal("image must not be text-attached as file content")
		}
	}
}

func TestImageMediaTypes(t *testing.T) {
	cases := map[string]string{
		"a/shot.PNG": "image/png", "b.jpg": "image/jpeg", "c.jpeg": "image/jpeg",
		"d.gif": "image/gif", "e.webp": "image/webp", "f.go": "", "g.txt": "",
	}
	for p, want := range cases {
		if got := imageMediaType(p); got != want {
			t.Errorf("imageMediaType(%q) = %q, want %q", p, got, want)
		}
		if isImagePath(p) != (want != "") {
			t.Errorf("isImagePath(%q) inconsistent", p)
		}
	}
}

func TestAttachImagesQueue(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "diagram.jpg"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "ok"}}}
	s := New(fp, nil, Options{Root: root, Out: io.Discard})
	if err := s.AttachImages([]string{"diagram.jpg"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachImages([]string{"nope.txt"}); err == nil {
		t.Fatal("non-image must be rejected")
	}
	if err := s.AttachImages([]string{"missing.png"}); err == nil {
		t.Fatal("missing image must be rejected")
	}
	if _, err := s.Ask(context.Background(), "what is this"); err != nil {
		t.Fatal(err)
	}
	last := s.msgs[len(s.msgs)-2] // [user task] [assistant reply]
	if last.Role != "user" || len(last.Images) != 1 || last.Images[0].MediaType != "image/jpeg" {
		t.Fatalf("queued image must ride the next task: %+v", last)
	}
	if len(s.pendingImages) != 0 {
		t.Fatal("queue must drain after the task")
	}
}
