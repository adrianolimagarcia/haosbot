package textutil

import (
	"reflect"
	"testing"
)

// TestImagePlaceholderText pins image_placeholder_text (helpers.py:384).
//
// The falsy case is the whole function: a None path and an empty path both
// produce the bare marker, and a path is used verbatim with no escaping.
func TestImagePlaceholderText(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"", "[image]"},
		{"/home/user/.nanobot/media/websocket/clip.mp4", "[image: /home/user/.nanobot/media/websocket/clip.mp4]"},
		{"0", "[image: 0]"},
		{" ", "[image:  ]"},
	}
	for _, tc := range cases {
		if got := ImagePlaceholderText(tc.path); got != tc.want {
			t.Errorf("ImagePlaceholderText(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestContentWithMediaBreadcrumbs covers the four ways the reference declines to
// build breadcrumbs, and the two ways it builds them.
//
// The declined cases return the content VALUE UNCHANGED, not a stringified copy
// of it, which is why the return type is `any`: a multimodal content list must
// survive untouched.
func TestContentWithMediaBreadcrumbs(t *testing.T) {
	blocks := []any{map[string]any{"type": "text"}}
	cases := []struct {
		name    string
		role    string
		content any
		media   any
		want    any
	}{
		{"appends", "user", "hello", []any{"/a.png"}, "hello\n[image: /a.png]"},
		{"media_only", "user", "", []any{"/a.png"}, "[image: /a.png]"},
		{"empty_media", "user", "hello", []any{}, "hello"},
		{"empty_media_entry", "user", "hello", []any{""}, "hello"},
		{"empty_media_and_content", "user", "", []any{""}, ""},
		{"non_user_role", "assistant", "hello", []any{"/a.png"}, "hello"},
		{"empty_role", "", "hello", []any{"/a.png"}, "hello"},
		{"non_string_content", "user", blocks, []any{"/a.png"}, blocks},
		{"nil_content", "user", nil, []any{"/a.png"}, nil},
		{"zero_content", "user", 0, []any{"/a.png"}, 0},
		{"media_not_a_list", "user", "hello", "notalist", "hello"},
		{"media_nil", "user", "hello", nil, "hello"},
		{"non_string_entries", "user", "hello", []any{nil, 1, "/a.png"}, "hello\n[image: /a.png]"},
		{"skips_empty_entries", "user", "hello", []any{"a", "", "b"}, "hello\n[image: a]\n[image: b]"},
		{"whitespace_content", "user", "  ", []any{"/a.png"}, "  \n[image: /a.png]"},
		{"string_slice_media", "user", "hello", []string{"/a.png"}, "hello\n[image: /a.png]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ContentWithMediaBreadcrumbs(tc.role, tc.content, tc.media)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ContentWithMediaBreadcrumbs(%q, %#v, %#v)\n got: %#v\nwant: %#v",
					tc.role, tc.content, tc.media, got, tc.want)
			}
		})
	}
}

// TestContentWithMediaBreadcrumbsDoesNotMutateMedia checks the caller's slice is
// not reused as storage for the breadcrumbs.
func TestContentWithMediaBreadcrumbsDoesNotMutateMedia(t *testing.T) {
	media := []any{"/a.png"}
	_ = ContentWithMediaBreadcrumbs("user", "hi", media)
	if !reflect.DeepEqual(media, []any{"/a.png"}) {
		t.Errorf("media was modified: %#v", media)
	}
}
