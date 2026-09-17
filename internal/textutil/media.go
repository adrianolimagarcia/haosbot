package textutil

import "strings"

// ImagePlaceholderText ports image_placeholder_text (utils/helpers.py:384-386):
//
//	return f"[image: {path}]" if path else empty
//
// The reference's `empty` keyword defaults to "[image]" and is never passed
// another value anywhere in the ported scope, so it is not exposed as a
// parameter; a caller that needs a different fallback would have to add it.
//
// `path` is typed as string rather than *string because the reference's two
// falsy cases (None and "") produce the same output.
func ImagePlaceholderText(path string) string {
	if path != "" {
		return "[image: " + path + "]"
	}
	return "[image]"
}

// ContentWithMediaBreadcrumbs ports content_with_media_breadcrumbs
// (utils/helpers.py:389-405): append persisted user-media breadcrumbs to
// plain-text content.
//
// The return type is `any` because the reference returns *content itself,
// unchanged and unconverted*, in every case where the breadcrumbs do not
// apply:
//
//	role != "user"  OR  content is not a str  OR  media is not a list
//
// In particular a multimodal content list (the common case for a user turn
// with an attachment) passes straight through, so a caller must not assume the
// result is a string.
//
// `role` is a string here while the reference accepts any value: the only thing
// the reference does with it is compare it against "user", and every non-string
// value compares unequal exactly like a non-"user" string does.
//
// `media` is accepted as []any (the shape a decoded JSON array has, which is
// what a session message holds) or as []string (the shape Go callers naturally
// have). Anything else — including nil — is "not a list" and returns content
// unchanged. Entries that are not non-empty strings are skipped, mirroring the
// reference's `isinstance(path, str) and path` filter.
func ContentWithMediaBreadcrumbs(role string, content any, media any) any {
	text, isText := content.(string)
	if role != "user" || !isText {
		return content
	}

	var paths []string
	switch list := media.(type) {
	case []any:
		paths = make([]string, 0, len(list))
		for _, item := range list {
			if path, ok := item.(string); ok {
				paths = append(paths, path)
			}
		}
	case []string:
		paths = list
	default:
		return content
	}

	var b strings.Builder
	for _, path := range paths {
		if path == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ImagePlaceholderText(path))
	}
	if b.Len() == 0 {
		return content
	}
	if text == "" {
		return b.String()
	}
	return text + "\n" + b.String()
}
