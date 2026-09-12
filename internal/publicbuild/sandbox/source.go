package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const maximumSourceInodes = 32_768

type GitHubSourceFetcher struct {
	Client *http.Client
}

func (fetcher *GitHubSourceFetcher) Fetch(
	ctx context.Context,
	repository string,
	commit string,
	destination string,
	maximumBytes int64,
) error {
	if maximumBytes <= 0 {
		return errors.New("source archive byte limit must be positive")
	}
	owner, name, err := githubRepositoryParts(repository)
	if err != nil {
		return err
	}
	if !commitPattern.MatchString(commit) {
		return errors.New("source commit must be an immutable 40 or 64 character hexadecimal digest")
	}
	archiveURL := "https://codeload.github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/tar.gz/" + commit
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return fmt.Errorf("create immutable source request: %w", err)
	}
	client := fetcher.Client
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.TLSHandshakeTimeout = 5 * time.Second
		client = &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("immutable GitHub source download redirected")
			},
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download immutable source archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("immutable GitHub source download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maximumBytes {
		return fmt.Errorf("compressed source archive exceeds %d bytes", maximumBytes)
	}
	compressed := &boundedReader{reader: response.Body, remaining: maximumBytes}
	gzipReader, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open immutable source archive: %w", err)
	}
	defer gzipReader.Close()
	if err := extractSourceTarWithInodeLimit(
		destination,
		&boundedReader{reader: gzipReader, remaining: maximumBytes},
		sourceInodeLimit(maximumBytes),
	); err != nil {
		return err
	}
	if compressed.exceeded {
		return fmt.Errorf("compressed source archive exceeds %d bytes", maximumBytes)
	}
	return nil
}

type boundedReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (reader *boundedReader) Read(buffer []byte) (int, error) {
	if reader.remaining < 0 {
		reader.exceeded = true
		return 0, errors.New("byte limit exceeded")
	}
	if int64(len(buffer)) > reader.remaining+1 {
		buffer = buffer[:reader.remaining+1]
	}
	read, err := reader.reader.Read(buffer)
	reader.remaining -= int64(read)
	if reader.remaining < 0 {
		reader.exceeded = true
		return read, errors.New("byte limit exceeded")
	}
	return read, err
}

func extractSourceTar(destination string, source io.Reader) error {
	return extractSourceTarWithInodeLimit(destination, source, maximumSourceInodes)
}

func extractSourceTarWithInodeLimit(destination string, source io.Reader, inodeLimit int) error {
	if inodeLimit <= 0 || inodeLimit > maximumSourceInodes {
		return errors.New("source archive inode limit is invalid")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create source staging directory: %w", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open source staging directory: %w", err)
	}
	defer root.Close()
	reader := tar.NewReader(source)
	seen := make(map[string]struct{})
	inodes := make(map[string]struct{})
	archiveRoot := ""
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read immutable source archive: %w", err)
		}
		entries++
		if entries > inodeLimit {
			return fmt.Errorf("source archive exceeds its %d-inode budget", inodeLimit)
		}
		cleanName, rootName, err := cleanArchiveName(header.Name)
		if err != nil {
			return err
		}
		if archiveRoot == "" {
			archiveRoot = rootName
		}
		if rootName != archiveRoot {
			return errors.New("source archive contains more than one top-level directory")
		}
		if cleanName == "" {
			if header.Typeflag != tar.TypeDir {
				return errors.New("source archive top-level entry is not a directory")
			}
			continue
		}
		if _, duplicate := seen[cleanName]; duplicate {
			return fmt.Errorf("source archive repeats path %q", cleanName)
		}
		seen[cleanName] = struct{}{}
		for current := cleanName; current != "." && current != ""; current = path.Dir(current) {
			if _, exists := inodes[current]; !exists {
				inodes[current] = struct{}{}
				if len(inodes) > inodeLimit {
					return fmt.Errorf("source archive exceeds its %d-inode budget", inodeLimit)
				}
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(cleanName, 0o700); err != nil {
				return fmt.Errorf("create source directory %q: %w", cleanName, err)
			}
		case tar.TypeReg, 0:
			if header.Size < 0 {
				return fmt.Errorf("source archive file %q has a negative size", cleanName)
			}
			parent := path.Dir(cleanName)
			if parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return fmt.Errorf("create source directory %q: %w", parent, err)
				}
			}
			mode := os.FileMode(0o400)
			if header.FileInfo().Mode()&0o111 != 0 {
				mode = 0o500
			}
			file, err := root.OpenFile(cleanName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return fmt.Errorf("create source file %q: %w", cleanName, err)
			}
			copyErr := copyExactly(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract source file %q: %w", cleanName, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close source file %q: %w", cleanName, closeErr)
			}
		case tar.TypeSymlink:
			if err := ValidateSourceSymlink(cleanName, header.Linkname); err != nil {
				return fmt.Errorf("source archive symlink %q: %w", cleanName, err)
			}
			parent := path.Dir(cleanName)
			if parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return fmt.Errorf("create source directory %q: %w", parent, err)
				}
			}
			if err := root.Symlink(header.Linkname, cleanName); err != nil {
				return fmt.Errorf("create source symlink %q: %w", cleanName, err)
			}
		default:
			return fmt.Errorf("source archive path %q uses forbidden tar type %d", cleanName, header.Typeflag)
		}
	}
	if entries == 0 || archiveRoot == "" {
		return errors.New("source archive is empty")
	}
	return filepath.WalkDir(destination, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == destination || !entry.IsDir() {
			return nil
		}
		return os.Chmod(filePath, 0o500)
	})
}

// ValidateSourceSymlink accepts only relative repository links whose
// normalized target remains inside the sealed source tree.
func ValidateSourceSymlink(relativeName, linkTarget string) error {
	if relativeName == "" || path.Clean(relativeName) != relativeName ||
		linkTarget == "" || strings.ContainsAny(linkTarget, "\\\x00") || path.IsAbs(linkTarget) {
		return errors.New("link must be a non-empty relative path")
	}
	resolved := path.Clean(path.Join(path.Dir(relativeName), linkTarget))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return errors.New("link target escapes the source root")
	}
	return nil
}

func sourceInodeLimit(maximumBytes int64) int {
	// A tar header consumes at least 512 expanded bytes. The hard cap also
	// protects WorkRoot filesystems with a small inode pool when callers allow a
	// large source byte budget.
	limit := maximumBytes / 512
	if limit < 1 {
		return 1
	}
	if limit > maximumSourceInodes {
		return maximumSourceInodes
	}
	return int(limit)
}

func cleanArchiveName(name string) (string, string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return "", "", errors.New("source archive contains an unsafe path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", errors.New("source archive contains an unsafe path")
	}
	parts := strings.Split(clean, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return "", "", errors.New("source archive contains an unsafe top-level directory")
	}
	if len(parts) == 1 {
		return "", parts[0], nil
	}
	relative := path.Join(parts[1:]...)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", "", errors.New("source archive contains an unsafe path")
	}
	return relative, parts[0], nil
}

func githubRepositoryParts(repository string) (string, string, error) {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("source repository must be a canonical HTTPS github.com URL")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] != strings.ToLower(parts[0]) || parts[1] != strings.ToLower(parts[1]) {
		return "", "", errors.New("source repository must name one lowercase GitHub owner and repository")
	}
	return parts[0], parts[1], nil
}
