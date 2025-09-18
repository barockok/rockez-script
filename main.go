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
    "os"
    "path"
    "path/filepath"
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
        srcDir       string
		dstCredPath  string
		dstBucket    string
        dstDir       string
		timeframeArg string
		grepArg      string
		maxWorkers   int
	)

	flag.StringVar(&srcCredPath, "src-service-account", "", "Path to source GCS service account JSON key file")
	flag.StringVar(&srcBucket, "src-bucket", "", "Source GCS bucket name containing the Elasticsearch snapshot repo")
    flag.StringVar(&srcDir, "src-dir", "", "Source local directory containing the Elasticsearch snapshot repo (alternative to --src-bucket)")
	flag.StringVar(&dstCredPath, "dest-service-account", "", "Path to destination GCS service account JSON key file")
	flag.StringVar(&dstBucket, "dest-bucket", "", "Destination GCS bucket to receive copy and restore request")
    flag.StringVar(&dstDir, "dest-dir", "", "Destination local directory to receive copy and restore request (alternative to --dest-bucket)")
	flag.StringVar(&timeframeArg, "timeframe", "", "Timeframe filter RFC3339/RFC3339 (e.g. 2025-09-01T00:00:00Z/2025-09-10T23:59:59Z)")
	flag.StringVar(&grepArg, "grep", "", "Substring or regex to match snapshot name or index names")
	flag.IntVar(&maxWorkers, "concurrency", 16, "Max concurrent copies")
	flag.Parse()

    // Validate source
    if srcDir == "" {
        if srcBucket == "" || srcCredPath == "" {
            log.Fatal("Provide either --src-dir or both --src-bucket and --src-service-account")
        }
    } else {
        if srcBucket != "" || srcCredPath != "" {
            log.Fatal("Use either local source (--src-dir) or GCS source (--src-bucket + --src-service-account), not both")
        }
    }
    // Validate destination
    if dstDir == "" {
        if dstBucket == "" || dstCredPath == "" {
            log.Fatal("Provide either --dest-dir or both --dest-bucket and --dest-service-account")
        }
    } else {
        if dstBucket != "" || dstCredPath != "" {
            log.Fatal("Use either local destination (--dest-dir) or GCS destination (--dest-bucket + --dest-service-account), not both")
        }
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

    // Build source repo
    var srcRepo Repo
    var srcClient *storage.Client
    if srcDir != "" {
        srcRepo = newFSRepo(srcDir)
    } else {
        srcClient, err = storage.NewClient(ctx, option.WithCredentialsFile(srcCredPath))
        if err != nil {
            log.Fatalf("failed to init source storage client: %v", err)
        }
        defer srcClient.Close()
        srcRepo = newGCSRepo(srcClient, srcBucket)
    }
    if err := srcRepo.EnsureAccessible(ctx); err != nil {
        log.Fatalf("source not accessible: %v", err)
    }

    // Build destination repo
    var dstRepo Repo
    var dstClient *storage.Client
    if dstDir != "" {
        dstRepo = newFSRepo(dstDir)
    } else {
        dstClient, err = storage.NewClient(ctx, option.WithCredentialsFile(dstCredPath))
        if err != nil {
            log.Fatalf("failed to init destination storage client: %v", err)
        }
        defer dstClient.Close()
        dstRepo = newGCSRepo(dstClient, dstBucket)
    }
    if err := dstRepo.EnsureAccessible(ctx); err != nil {
        log.Fatalf("destination not accessible: %v", err)
    }

    // Copy the entire repository to ensure restore-compatibility.
    if err := copyAllObjectsGeneric(ctx, srcRepo, dstRepo, maxWorkers); err != nil {
        log.Fatalf("failed to copy repository: %v", err)
    }

    // Attempt to determine the best snapshot within timeframe/grep for a restore request.
    selectedSnapshot, indicesPattern := selectSnapshotForRestoreGeneric(ctx, srcRepo, start, end, grepMatcher)
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
    if err := writeRestoreRequestGeneric(ctx, dstRepo, selectedSnapshot, req); err != nil {
		log.Fatalf("failed to write restore request: %v", err)
	}

    log.Printf("Done. Repository copied to %s. Restore request JSON written.", dstRepo.String())
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

func copyAllObjectsGeneric(ctx context.Context, src Repo, dst Repo, maxWorkers int) error {
    it, err := src.List(ctx)
    if err != nil {
        return err
    }
    objCh := make(chan *ObjectAttrs, maxWorkers*2)
    var wg sync.WaitGroup
    var copyErr error
    var mu sync.Mutex

    worker := func() {
        defer wg.Done()
        for attrs := range objCh {
            r, err := src.Reader(ctx, attrs.Name)
            if err != nil {
                mu.Lock()
                if copyErr == nil {
                    copyErr = fmt.Errorf("open src %s: %w", attrs.Name, err)
                }
                mu.Unlock()
                continue
            }
            w, err := dst.Writer(ctx, attrs.Name, attrs.ContentType)
            if err != nil {
                _ = r.Close()
                mu.Lock()
                if copyErr == nil {
                    copyErr = fmt.Errorf("open dst %s: %w", attrs.Name, err)
                }
                mu.Unlock()
                continue
            }
            if _, err := io.Copy(w, r); err != nil {
                mu.Lock()
                if copyErr == nil {
                    copyErr = fmt.Errorf("copy %s: %w", attrs.Name, err)
                }
                mu.Unlock()
            }
            _ = r.Close()
            if err := w.Close(); err != nil {
                mu.Lock()
                if copyErr == nil {
                    copyErr = fmt.Errorf("close dst %s: %w", attrs.Name, err)
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
            return err
        }
        objCh <- attrs
    }
    close(objCh)
    wg.Wait()
    return copyErr
}

func selectSnapshotForRestoreGeneric(ctx context.Context, repo Repo, start, end time.Time, grep *regexp.Regexp) (string, string) {
	// Defaults
	indicesPattern := "*"
	selected := ""

	// Try reading index.latest and index-N
    idxNName, err := readIndexLatestGeneric(ctx, repo)
	if err != nil {
		log.Printf("info: cannot read index.latest/index-N: %v", err)
		return selected, indicesPattern
	}
    idx, err := readIndexFileGeneric(ctx, repo, idxNName)
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

func readIndexLatestGeneric(ctx context.Context, repo Repo) (string, error) {
    r, err := repo.Reader(ctx, "index.latest")
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

func readIndexFileGeneric(ctx context.Context, repo Repo, name string) (indexFile, error) {
    r, err := repo.Reader(ctx, name)
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

func writeRestoreRequestGeneric(ctx context.Context, repo Repo, snapshot string, body restoreRequestBody) error {
    when := time.Now().UTC().Format("20060102T150405Z")
    base := "restore-requests"
    name := fmt.Sprintf("%s/restore_%s.json", base, when)
    if snapshot != "" {
        name = fmt.Sprintf("%s/restore_%s_%s.json", base, sanitizeForPath(snapshot), when)
    }
    w, err := repo.Writer(ctx, name, "application/json")
    if err != nil {
        return err
    }
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
    w2, err := repo.Writer(ctx, textName, "text/plain")
    if err != nil {
        return err
    }
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

// Repository abstraction

type Repo interface {
    List(ctx context.Context) (ObjectIterator, error)
    Reader(ctx context.Context, name string) (io.ReadCloser, error)
    Writer(ctx context.Context, name string, contentType string) (io.WriteCloser, error)
    EnsureAccessible(ctx context.Context) error
    String() string
}

type ObjectAttrs struct {
    Name        string
    ContentType string
}

type ObjectIterator interface {
    Next() (*ObjectAttrs, error)
}

// GCS implementation
type gcsRepo struct {
    client *storage.Client
    bucket string
}

func newGCSRepo(client *storage.Client, bucket string) Repo {
    return &gcsRepo{client: client, bucket: bucket}
}

func (r *gcsRepo) EnsureAccessible(ctx context.Context) error {
    _, err := r.client.Bucket(r.bucket).Attrs(ctx)
    return err
}

func (r *gcsRepo) List(ctx context.Context) (ObjectIterator, error) {
    it := r.client.Bucket(r.bucket).Objects(ctx, &storage.Query{})
    return &gcsObjectIterator{it: it}, nil
}

func (r *gcsRepo) Reader(ctx context.Context, name string) (io.ReadCloser, error) {
    return r.client.Bucket(r.bucket).Object(name).NewReader(ctx)
}

func (r *gcsRepo) Writer(ctx context.Context, name string, contentType string) (io.WriteCloser, error) {
    w := r.client.Bucket(r.bucket).Object(name).NewWriter(ctx)
    if contentType != "" {
        if sw, ok := w.(*storage.Writer); ok {
            sw.ContentType = contentType
        }
    }
    return w, nil
}

func (r *gcsRepo) String() string {
    return "gs://" + r.bucket
}

type gcsObjectIterator struct {
    it *storage.ObjectIterator
}

func (it *gcsObjectIterator) Next() (*ObjectAttrs, error) {
    attrs, err := it.it.Next()
    if err != nil {
        return nil, err
    }
    return &ObjectAttrs{Name: attrs.Name, ContentType: attrs.ContentType}, nil
}

// Filesystem implementation
type fsRepo struct {
    root string
}

func newFSRepo(root string) Repo {
    return &fsRepo{root: root}
}

func (r *fsRepo) EnsureAccessible(ctx context.Context) error {
    info, err := os.Stat(r.root)
    if err != nil {
        return err
    }
    if !info.IsDir() {
        return fmt.Errorf("not a directory: %s", r.root)
    }
    return nil
}

func (r *fsRepo) List(ctx context.Context) (ObjectIterator, error) {
    var names []string
    err := filepath.WalkDir(r.root, func(p string, d os.DirEntry, err error) error {
        if err != nil {
            return err
        }
        if d.IsDir() {
            return nil
        }
        rel, err := filepath.Rel(r.root, p)
        if err != nil {
            return err
        }
        // Normalize to forward slashes
        rel = filepath.ToSlash(rel)
        names = append(names, rel)
        return nil
    })
    if err != nil {
        return nil, err
    }
    return &fsObjectIterator{names: names, idx: 0}, nil
}

func (r *fsRepo) Reader(ctx context.Context, name string) (io.ReadCloser, error) {
    p := filepath.Join(r.root, filepath.FromSlash(name))
    return os.Open(p)
}

func (r *fsRepo) Writer(ctx context.Context, name string, contentType string) (io.WriteCloser, error) {
    p := filepath.Join(r.root, filepath.FromSlash(name))
    if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
        return nil, err
    }
    return os.Create(p)
}

func (r *fsRepo) String() string {
    return r.root
}

type fsObjectIterator struct {
    names []string
    idx   int
}

func (it *fsObjectIterator) Next() (*ObjectAttrs, error) {
    if it.idx >= len(it.names) {
        return nil, iterator.Done
    }
    name := it.names[it.idx]
    it.idx++
    return &ObjectAttrs{Name: name, ContentType: ""}, nil
}
