# Mastodon To Hugo

A `go` program that converts a [Mastodon Archive](https://docs.joinmastodon.org/user/moving/)
to a set of [Hugo](https://gohugo.io) Markdown pages. Markdown page contents
are expressed as [text/template](https://pkg.go.dev/text/template) frontmatter and
toot templates.

## Features

- All self-reply threads are appended to the primary Toot's markdown file
- Toot attachments are copied to the page bundle directory. Attachments can be referenced in the
toot template via the `ActivityObjectAttachment.BaseFilename` field value
- ActivityFeed tags include a leading `#` character. This is stripped from the `ActivityObjectTag.Name` field
- Only `Hashtag` tag types are deserialized

## Usage

See the [blog post](https://mweagle.net/posts/2024/03/mastodon-to-hugo/) for information
on customizing the script for your Mastodon profile and Hugo site theme.
