package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type snapshotMetadata struct {
	Snapshot          string   `json:"snapshot"`
	UUID             string   `json:"uuid"`
	Indices          []string `json:"indices"`
	StartTimeMillis  int64    `json:"start_time_millis"`
	EndTimeMillis    int64    `json:"end_time_millis"`
}

type indexFile struct {
	Snapshots []snapshotMetadata `json:"snapshots"`
}

type restoreRequestBody struct {
	Indices            string `json:"indices,omitempty"`
	IgnoreUnavailable  bool   `json:"ignore_unavailable"`
	IncludeGlobalState bool   `json:"include_global_state"`
	IncludeAliases     bool   `json:"include_aliases"`
	Partial            bool   `json:"partial"`
}

func main() {
	var (
		srcCredPath  string
		srcBucket    string
		dstCredPath  string
		dstBucket    string
		timeframeArg string
		grepArg      string
		maxWorkers   int
	)

	flag.StringVar(&srcCredPath, "src-service-account", "", "Path to source GCS service account JSON key file")
	flag.StringVar(&srcBucket, "src-bucket", "", "Source GCS bucket name containing the Elasticsearch snapshot repo")
	flag.StringVar(&dstCredPath, "dest-service-account", "", "Path to destination GCS service account JSON key file")
	flag.StringVar(&dstBucket, "dest-bucket", "", "Destination GCS bucket to receive copy and restore request")
	flag.StringVar(&timeframeArg, "timeframe", "", "Timeframe filter RFC3339/RFC3339 (e.g. 2025-09-01T00:00:00Z/2025-09-10T23:59:59Z)")
	flag.StringVar(&grepArg, "grep", "", "Substring or regex to match snapshot name or index names")
	flag.IntVar(&maxWorkers, "concurrency", 16, "Max concurrent copies")
	flag.Parse()

	if srcCredPath == "" || srcBucket == "" || dstCredPath == "" || dstBucket == "" {
		log.Fatal("All of --src-service-account, --src-bucket, --dest-service-account, --dest-bucket are required")
	}

	var (
		start time.Time
		end   time.Time
		err   error
	)
	if timeframeArg != "" {
		start, end, err = parseTimeframe(timeframeArg)
		if err != nil {
			log.Fatalf("invalid --timeframe: %v", err)
		}
	}

	var grepMatcher *regexp.Regexp
	if grepArg != "" {
		// Treat input as regex if it compiles, else escape and use as literal substring match via regex
		grepMatcher, err = regexp.Compile(grepArg)
		if err != nil {
			grepMatcher = regexp.MustCompile(regexp.QuoteMeta(grepArg))
		}
	}

	ctx := context.Background()
	srcClient, err := storage.NewClient(ctx, option.WithCredentialsFile(srcCredPath))
	if err != nil {
		log.Fatalf("failed to init source storage client: %v", err)
	}
	defer srcClient.Close()

	dstClient, err := storage.NewClient(ctx, option.WithCredentialsFile(dstCredPath))
	if err != nil {
		log.Fatalf("failed to init destination storage client: %v", err)
	}
	defer dstClient.Close()

	// Copy the entire repository to ensure restore-compatibility.
	if err := copyAllObjects(ctx, srcClient, srcBucket, dstClient, dstBucket, maxWorkers); err != nil {
		log.Fatalf("failed to copy repository: %v", err)
	}

	// Attempt to determine the best snapshot within timeframe/grep for a restore request.
	selectedSnapshot, indicesPattern := selectSnapshotForRestore(ctx, srcClient, srcBucket, start, end, grepMatcher)
	if selectedSnapshot == "" {
		log.Printf("warning: no snapshot matched timeframe/grep; generating generic restore request")
	}

	// Compose a restore request body and store it in the destination bucket.
	req := restoreRequestBody{
		Indices:            indicesPattern,
		IgnoreUnavailable:  true,
		IncludeGlobalState: false,
		IncludeAliases:     true,
		Partial:            false,
	}
	if err := writeRestoreRequest(ctx, dstClient, dstBucket, selectedSnapshot, req); err != nil {
		log.Fatalf("failed to write restore request: %v", err)
	}

	log.Printf("Done. Repository copied to gs://%s. Restore request JSON written.", dstBucket)
}

func parseTimeframe(tf string) (time.Time, time.Time, error) {
	parts := strings.Split(tf, "/")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("expected start/end RFC3339, got %q", tf)
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start time: %w", err)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1]))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid end time: %w", err)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("end must be after start")
	}
	return start, end, nil
}

func copyAllObjects(ctx context.Context, srcClient *storage.Client, srcBucket string, dstClient *storage.Client, dstBucket string, maxWorkers int) error {
	src := srcClient.Bucket(srcBucket)
	dst := dstClient.Bucket(dstBucket)

	// Ensure destination bucket exists (no-op if it already exists and we have perms to access)
	if _, err := dst.Attrs(ctx); err != nil {
		return fmt.Errorf("destination bucket %s not accessible: %w", dstBucket, err)
	}

	it := src.Objects(ctx, &storage.Query{})
	objCh := make(chan *storage.ObjectAttrs, maxWorkers*2)
	var wg sync.WaitGroup
	var copyErr error
	var mu sync.Mutex

	worker := func() {
		defer wg.Done()
		for attrs := range objCh {
			// Preserve object name
			dstObj := dst.Object(attrs.Name)
			srcObj := src.Object(attrs.Name)
			copier := dstObj.CopierFrom(srcObj)
			// Preserve metadata and contentType if possible
			copier.ContentType = attrs.ContentType
			if _, err := copier.Run(ctx); err != nil {
				mu.Lock()
				if copyErr == nil {
					copyErr = fmt.Errorf("copy %s: %w", attrs.Name, err)
				}
				mu.Unlock()
			}
		}
	}

	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go worker()
	}

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			close(objCh)
			wg.Wait()
			return fmt.Errorf("list objects: %w", err)
		}
		objCh <- attrs
	}
	close(objCh)
	wg.Wait()

	return copyErr
}

func selectSnapshotForRestore(ctx context.Context, client *storage.Client, bucket string, start, end time.Time, grep *regexp.Regexp) (string, string) {
	// Defaults
	indicesPattern := "*"
	selected := ""

	// Try reading index.latest and index-N
	idxNName, err := readIndexLatest(ctx, client, bucket)
	if err != nil {
		log.Printf("info: cannot read index.latest/index-N: %v", err)
		return selected, indicesPattern
	}
	idx, err := readIndexFile(ctx, client, bucket, idxNName)
	if err != nil {
		log.Printf("info: cannot read %s: %v", idxNName, err)
		return selected, indicesPattern
	}

	// Filter snapshots by timeframe and optional grep (against snapshot name or any index name)
	var candidates []snapshotMetadata
	for _, s := range idx.Snapshots {
		if start.IsZero() == false || end.IsZero() == false {
			if s.StartTimeMillis == 0 {
				continue
			}
			st := time.UnixMilli(s.StartTimeMillis)
			if st.Before(start) || st.After(end) {
				continue
			}
		}
		if grep != nil {
			matched := grep.MatchString(s.Snapshot)
			if !matched {
				for _, indexName := range s.Indices {
					if grep.MatchString(indexName) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		candidates = append(candidates, s)
	}

	if len(candidates) == 0 {
		return "", indicesPattern
	}

	// Pick the newest by start time
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].StartTimeMillis < candidates[j].StartTimeMillis })
	choice := candidates[len(candidates)-1]
	selected = choice.Snapshot

	// If grep was provided and matched indices, convert it to indices pattern for restore body
	if grep != nil {
		// Use the provided string as-is as a simple pattern if it doesn't look like a regex meta
		indicesPattern = grep.String()
	}

	return selected, indicesPattern
}

func readIndexLatest(ctx context.Context, client *storage.Client, bucket string) (string, error) {
	b := client.Bucket(bucket)
	r, err := b.Object("index.latest").NewReader(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return "", errors.New("empty index.latest")
	}
	line := strings.TrimSpace(scanner.Text())
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("index-%s", line), nil
}

func readIndexFile(ctx context.Context, client *storage.Client, bucket, name string) (indexFile, error) {
	b := client.Bucket(bucket)
	r, err := b.Object(name).NewReader(ctx)
	if err != nil {
		return indexFile{}, err
	}
	defer r.Close()
	var idx indexFile
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return indexFile{}, err
	}
	return idx, nil
}

func writeRestoreRequest(ctx context.Context, client *storage.Client, bucket, snapshot string, body restoreRequestBody) error {
	b := client.Bucket(bucket)
	when := time.Now().UTC().Format("20060102T150405Z")
	base := "restore-requests"
	name := fmt.Sprintf("%s/restore_%s.json", base, when)
	if snapshot != "" {
		name = fmt.Sprintf("%s/restore_%s_%s.json", base, sanitizeForPath(snapshot), when)
	}
	w := b.Object(name).NewWriter(ctx)
	w.ContentType = "application/json"
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	// Also write a small helper text with the REST call format
	cmd := buildRestoreCurl(snapshot)
	textName := strings.TrimSuffix(name, path.Ext(name)) + ".txt"
	w2 := b.Object(textName).NewWriter(ctx)
	w2.ContentType = "text/plain"
	if _, err := io.WriteString(w2, cmd+"\n"); err != nil {
		_ = w2.Close()
		return err
	}
	return w2.Close()
}

func buildRestoreCurl(snapshot string) string {
	if snapshot == "" {
		return "# Example: POST /_snapshot/{REPOSITORY}/{SNAPSHOT}/_restore -d @restore.json"
	}
	return fmt.Sprintf("# Run after registering GCS repo in Elasticsearch\n# Replace {REPOSITORY} with your repo name that points to gs:// in cluster\n# Assuming file is restore-request/restore_%s.json\n# curl -X POST $ES/_snapshot/{REPOSITORY}/%s/_restore -H 'Content-Type: application/json' --data-binary @restore.json", snapshot, snapshot)
}

func sanitizeForPath(name string) string {
	// Keep it simple: replace path separators and spaces
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "_")
	return replacer.Replace(name)
}
