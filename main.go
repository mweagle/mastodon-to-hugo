package main

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Mastodon archive structure
type Archive struct {
	OrderedItems []Activity `json:"orderedItems"`
}

type Activity struct {
	Published string          `json:"published"`
	Actor     string          `json:"actor"`
	Object    json.RawMessage `json:"object"`
}

type Note struct {
	ID         string       `json:"id"`
	URL        string       `json:"url"`
	Published  string       `json:"published"`
	Content    string       `json:"content"`
	Summary    *string      `json:"summary"`
	InReplyTo  *string      `json:"inReplyTo"`
	Sensitive  bool         `json:"sensitive"`
	Attachment []Attachment `json:"attachment,omitempty"`
	Tag        []Tag        `json:"tag,omitempty"`
	To         []string     `json:"to,omitempty"`
	Cc         []string     `json:"cc,omitempty"`
}

type Attachment struct {
	Type      string `json:"type"`
	MediaType string `json:"mediaType"`
	URL       string `json:"url"`
	Name      string `json:"name,omitempty"`
}

type Tag struct {
	Type string `json:"type"`
	Href string `json:"href"`
	Name string `json:"name"`
}

// Convert HTML content to plain text
func htmlToText(htmlContent string) string {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return htmlContent
	}

	var text strings.Builder
	var traverse func(*html.Node, bool)
	traverse = func(n *html.Node, skipChildren bool) {
		if skipChildren {
			return
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		if n.Type == html.ElementNode {
			if n.Data == "br" || n.Data == "p" {
				text.WriteString("\n")
			}
			if n.Data == "a" {
				// Check link type
				isHashtag := false
				isMention := false
				var href string
				for _, attr := range n.Attr {
					if attr.Key == "href" {
						href = attr.Val
					}
					if attr.Key == "class" {
						if strings.Contains(attr.Val, "hashtag") {
							isHashtag = true
						}
						if strings.Contains(attr.Val, "mention") {
							isMention = true
						}
					}
				}
				if isHashtag {
					// Skip hashtag links and their text content entirely
					return
				}
				if isMention {
					// For mentions, create a markdown link: [@username](profile_url)
					var mentionText string
					var extractText func(*html.Node)
					extractText = func(node *html.Node) {
						if node.Type == html.TextNode {
							mentionText += node.Data
						}
						for c := node.FirstChild; c != nil; c = c.NextSibling {
							extractText(c)
						}
					}
					extractText(n)

					if mentionText != "" && href != "" {
						text.WriteString("[")
						text.WriteString(mentionText)
						text.WriteString("](")
						text.WriteString(href)
						text.WriteString(")")
					}
					return
				}
				if href != "" {
					// For regular links, add the URL in markdown format
					text.WriteString("<")
					text.WriteString(href)
					text.WriteString(">")
					// Skip processing children of this link
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c, false)
		}
	}
	traverse(doc, false)

	// Clean up extra whitespace
	result := text.String()
	result = strings.TrimSpace(result)
	lines := strings.Split(result, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			// Wrap bare URLs in angle brackets for proper markdown
			if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
				line = "<" + line + ">"
			}
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, "\n\n")
}

// ActivityWithNote combines activity metadata with parsed note
type ActivityWithNote struct {
	Published string
	Object    Note
	Actor     string
}

// TootThread represents a root toot and its replies
type TootThread struct {
	Root    ActivityWithNote
	Replies []ActivityWithNote
}

// parseArchive reads and parses the outbox.json from the archive
func parseArchive(r *zip.Reader) (*Archive, error) {
	// Find outbox.json
	var outboxFile *zip.File
	for _, f := range r.File {
		if f.Name == "outbox.json" {
			outboxFile = f
			break
		}
	}

	if outboxFile == nil {
		return nil, fmt.Errorf("outbox.json not found in archive")
	}

	// Read outbox.json
	rc, err := outboxFile.Open()
	if err != nil {
		return nil, fmt.Errorf("error opening outbox.json: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("error reading outbox.json: %w", err)
	}

	var archive Archive
	if err := json.Unmarshal(data, &archive); err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	return &archive, nil
}

// extractMedia extracts all media attachments from the archive
func extractMedia(r *zip.Reader, mediaDir string) (map[string]string, error) {
	extractedMedia := make(map[string]string)

	for _, f := range r.File {
		if !strings.HasPrefix(f.Name, "media_attachments/") {
			continue
		}

		filename := filepath.Base(f.Name)
		destPath := filepath.Join(mediaDir, filename)

		if _, exists := extractedMedia[f.Name]; exists {
			continue
		}

		src, err := f.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening media file %s: %v\n", f.Name, err)
			continue
		}

		dst, err := os.Create(destPath)
		if err != nil {
			src.Close()
			fmt.Fprintf(os.Stderr, "Error creating media file %s: %v\n", destPath, err)
			continue
		}

		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()

		if err != nil {
			fmt.Fprintf(os.Stderr, "Error copying media file %s: %v\n", f.Name, err)
			continue
		}

		extractedMedia[f.Name] = filename
	}

	return extractedMedia, nil
}

// Stats tracks processing statistics
type Stats struct {
	TotalProcessed    int
	TootsOutput       int
	PrivateSkipped    int
	RepliesToOthers   int
	EmptyContent      int
}

// collectToots filters and collects toots from the archive
func collectToots(archive *Archive, stats *Stats) []ActivityWithNote {
	var allToots []ActivityWithNote

	for _, activity := range archive.OrderedItems {
		var note Note
		if err := json.Unmarshal(activity.Object, &note); err != nil {
			continue
		}

		stats.TotalProcessed++

		if note.Content == "" {
			stats.EmptyContent++
			continue
		}

		// Skip private/direct messages (only include public posts)
		// Check both 'to' and 'cc' fields for Public URI
		publicURI := "https://www.w3.org/ns/activitystreams#Public"
		isPublic := slices.Contains(note.To, publicURI) || slices.Contains(note.Cc, publicURI)
		if !isPublic {
			stats.PrivateSkipped++
			continue
		}

		// Skip replies to other users (keep only original toots and self-replies)
		if note.InReplyTo != nil && *note.InReplyTo != "" {
			if !strings.Contains(*note.InReplyTo, activity.Actor) {
				stats.RepliesToOthers++
				continue
			}
		}

		allToots = append(allToots, ActivityWithNote{
			Published: activity.Published,
			Object:    note,
			Actor:     activity.Actor,
		})
		stats.TootsOutput++
	}

	return allToots
}

// buildThreads organizes toots into threads (root toots with their replies)
func buildThreads(allToots []ActivityWithNote) map[string][]TootThread {
	threadsByDate := make(map[string][]TootThread)

	// Find replies recursively
	var findReplies func(parentID string) []ActivityWithNote
	findReplies = func(parentID string) []ActivityWithNote {
		var replies []ActivityWithNote
		for _, candidate := range allToots {
			if candidate.Object.InReplyTo != nil && *candidate.Object.InReplyTo == parentID {
				replies = append(replies, candidate)
				replies = append(replies, findReplies(candidate.Object.ID)...)
			}
		}
		sort.Slice(replies, func(i, j int) bool {
			return replies[i].Published < replies[j].Published
		})
		return replies
	}

	// Build threads from root toots
	// A root toot is either:
	// 1. A toot with no InReplyTo (original toot)
	// 2. A toot replying to someone else (reply to external user)
	for _, toot := range allToots {
		// Skip if this is a self-reply (will be included as part of another thread)
		if toot.Object.InReplyTo != nil && *toot.Object.InReplyTo != "" {
			isSelfReply := strings.Contains(*toot.Object.InReplyTo, toot.Actor)
			if isSelfReply {
				continue
			}
		}

		thread := TootThread{
			Root:    toot,
			Replies: findReplies(toot.Object.ID),
		}

		t, err := time.Parse(time.RFC3339, toot.Published)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing date %s: %v\n", toot.Published, err)
			continue
		}

		dateKey := t.Format("2006-01-02")
		threadsByDate[dateKey] = append(threadsByDate[dateKey], thread)
	}

	return threadsByDate
}

// writeToot writes a single toot to the file
func writeToot(f *os.File, toot ActivityWithNote, headerLevel string, extractedMedia map[string]string) {
	// Convert HTML content to text
	content := htmlToText(toot.Object.Content)

	// Only write header for root toots (H2), not for replies (H3)
	if headerLevel == "##" {
		// Use first line of content as header, or timestamp if content is too long
		// First, get a single-line version by replacing all newlines with spaces
		singleLineContent := strings.ReplaceAll(content, "\n", " ")

		headerText := singleLineContent
		if len(singleLineContent) > 100 {
			// Truncate if too long
			headerText = singleLineContent[:97] + "..."
		}

		// Write header
		fmt.Fprintf(f, "%s %s\n\n", headerLevel, headerText)
	}

	// Write full content
	fmt.Fprintf(f, "%s\n", content)

	// Add attachments
	if len(toot.Object.Attachment) > 0 {
		fmt.Fprintf(f, "\n")
		for _, att := range toot.Object.Attachment {
			archivePath := strings.TrimPrefix(att.URL, "/")

			if filename, exists := extractedMedia[archivePath]; exists {
				isImage := strings.HasPrefix(att.MediaType, "image/")
				relPath := "/mastodon/media/" + filename

				if isImage {
					altText := att.Name
					if altText == "" {
						altText = "attachment"
					}
					fmt.Fprintf(f, "![%s](%s)\n", altText, relPath)
				} else {
					linkText := att.Name
					if linkText == "" {
						linkText = filename
					}
					fmt.Fprintf(f, "[%s](%s)\n", linkText, relPath)
				}
			} else {
				fmt.Fprintf(f, "[Attachment: %s](%s)\n", att.MediaType, att.URL)
			}
		}
	}

	// Add content warning
	if toot.Object.Summary != nil && *toot.Object.Summary != "" {
		fmt.Fprintf(f, "\n*Content Warning: %s*\n", *toot.Object.Summary)
	}

	// Add hashtags
	if len(toot.Object.Tag) > 0 {
		var hashtags []string
		for _, tag := range toot.Object.Tag {
			if tag.Type == "Hashtag" {
				hashtags = append(hashtags, tag.Name)
			}
		}
		if len(hashtags) > 0 {
			fmt.Fprintf(f, "\n<small><b>Tags:</b> ")
			for i, tag := range hashtags {
				if i > 0 {
					fmt.Fprintf(f, ", ")
				}
				fmt.Fprintf(f, "`%s`", tag)
			}
			fmt.Fprintf(f, "</small>\n")
		}
	}

	// Add Mastodon source link at the end
	fmt.Fprintf(f, "\n##### [Mastodon Source 🐘](%s)\n", toot.Object.URL)
}

// writeMarkdownFiles generates markdown files for all threads
func writeMarkdownFiles(threadsByDate map[string][]TootThread, outputDir string, extractedMedia map[string]string) error {
	var dates []string
	for date := range threadsByDate {
		dates = append(dates, date)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	generatedAt := time.Now().Format(time.RFC3339)

	for _, date := range dates {
		threads := threadsByDate[date]

		sort.Slice(threads, func(i, j int) bool {
			return threads[i].Root.Published > threads[j].Root.Published
		})

		// Parse date to extract year for subdirectory
		dateObj, _ := time.Parse("2006-01-02", date)
		year := dateObj.Format("2006")

		// Create year subdirectory
		yearDir := filepath.Join(outputDir, year)
		if err := os.MkdirAll(yearDir, 0755); err != nil {
			return fmt.Errorf("error creating year directory %s: %w", yearDir, err)
		}

		filename := filepath.Join(yearDir, date+".md")
		f, err := os.Create(filename)
		if err != nil {
			return fmt.Errorf("error creating file %s: %w", filename, err)
		}

		// Write frontmatter
		fmt.Fprintf(f, "---\n")
		fmt.Fprintf(f, "title: \"Mastodon - %s\"\n", date)
		fmt.Fprintf(f, "description: \"\"\n")
		fmt.Fprintf(f, "image: \"/images/mastodon.png\"\n")
		fmt.Fprintf(f, "date: %sT00:00:00Z\n", date)
		fmt.Fprintf(f, "lastmod: %sT00:00:00Z\n", date)
		fmt.Fprintf(f, "tags: [\"Social Media\"]\n")
		fmt.Fprintf(f, "categories: [\"mastodon\"]\n")
		fmt.Fprintf(f, "# generated: %s\n", generatedAt)
		fmt.Fprintf(f, "---\n\n")

		fmt.Fprintf(f, "# Toots from %s\n\n", date)

		tootCount := 0
		for _, thread := range threads {
			writeToot(f, thread.Root, "##", extractedMedia)
			tootCount++

			for _, reply := range thread.Replies {
				fmt.Fprintf(f, "\n")
				writeToot(f, reply, "###", extractedMedia)
				tootCount++
			}

			fmt.Fprintf(f, "\n---\n\n")
		}

		f.Close()
		fmt.Printf("Created %s with %d toots\n", filename, tootCount)
	}

	fmt.Printf("\nProcessed %d dates\n", len(dates))
	return nil
}

func main() {
	startTime := time.Now()

	// Define command-line flags
	archivePath := flag.String("archivePath", "", "Path to the Mastodon archive ZIP file (required)")
	outputDir := flag.String("output", "", "Path to the output directory for markdown files (required)")
	flag.Parse()

	// Validate required arguments
	if *archivePath == "" || *outputDir == "" {
		fmt.Fprintf(os.Stderr, "Error: Both --archivePath and --output are required\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  mastodon-to-hugo --archivePath <path-to-zip> --output <output-directory>\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Check if archive file exists
	if _, err := os.Stat(*archivePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: Archive file does not exist: %s\n", *archivePath)
		os.Exit(1)
	}

	mediaDir := filepath.Join(*outputDir, "media")

	// Create output directories
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating media directory: %v\n", err)
		os.Exit(1)
	}

	// Open the zip archive
	r, err := zip.OpenReader(*archivePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening archive: %v\n", err)
		os.Exit(1)
	}
	defer r.Close()

	// Parse archive
	archive, err := parseArchive(&r.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Extract media
	extractedMedia, err := extractMedia(&r.Reader, mediaDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error extracting media: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Extracted %d media files\n", len(extractedMedia))

	// Collect and organize toots
	stats := &Stats{}
	allToots := collectToots(archive, stats)
	threadsByDate := buildThreads(allToots)

	// Write markdown files
	if err := writeMarkdownFiles(threadsByDate, *outputDir, extractedMedia); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing markdown files: %v\n", err)
		os.Exit(1)
	}

	// Print summary statistics
	elapsed := time.Since(startTime)
	fmt.Println("\n=== Summary Statistics ===")
	fmt.Printf("Total time:              %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Total items processed:   %d\n", stats.TotalProcessed)
	fmt.Printf("Toots output:            %d\n", stats.TootsOutput)
	fmt.Printf("Toots omitted:           %d\n", stats.PrivateSkipped+stats.RepliesToOthers+stats.EmptyContent)
	fmt.Printf("  - Private/DMs:         %d\n", stats.PrivateSkipped)
	fmt.Printf("  - Replies to others:   %d\n", stats.RepliesToOthers)
	fmt.Printf("  - Empty content:       %d\n", stats.EmptyContent)
}
