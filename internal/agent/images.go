package agent

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/provasign/mason/internal/provider"
)

// Image input: a task that names an image file (or images queued via
// --image) gets it ATTACHED by the harness — base64 in the same user
// message — so vision-capable models see the screenshot/diagram the user is
// talking about. Attachment is deterministic; whether the model can use it
// depends on the model (non-vision models will say so or ignore it).

const maxImageBytes = 8 << 20 // request-size sanity bound per image

func imageMediaType(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

func isImagePath(p string) bool { return imageMediaType(p) != "" }

// loadImage reads and encodes one image, root-confined.
func (s *Session) loadImage(rel string) (provider.Image, error) {
	abs, err := s.inRoot(rel)
	if err != nil {
		return provider.Image{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return provider.Image{}, err
	}
	if len(b) > maxImageBytes {
		return provider.Image{}, fmt.Errorf("%s is %d bytes — over the %d image bound", rel, len(b), maxImageBytes)
	}
	return provider.Image{MediaType: imageMediaType(rel),
		DataB64: base64.StdEncoding.EncodeToString(b)}, nil
}

// AttachImages queues images (repo-relative or absolute-in-root paths) for
// the next task — the --image flag.
func (s *Session) AttachImages(paths []string) error {
	for _, p := range paths {
		if !isImagePath(p) {
			return fmt.Errorf("%s: not a supported image (png/jpg/gif/webp)", p)
		}
		img, err := s.loadImage(p)
		if err != nil {
			return err
		}
		s.pendingImages = append(s.pendingImages, img)
		s.pendingImageNames = append(s.pendingImageNames, p)
	}
	return nil
}

// imagesMentioned resolves image files the task refers to, the same way
// filesMentioned resolves source files: exact paths first, then basename
// search (bounded). Max 3.
func (s *Session) imagesMentioned(task string) (imgs []provider.Image, names []string) {
	for _, rel := range s.pathsMentioned(task, isImagePath, 3) {
		img, err := s.loadImage(rel)
		if err != nil {
			continue
		}
		imgs = append(imgs, img)
		names = append(names, rel)
	}
	return imgs, names
}
