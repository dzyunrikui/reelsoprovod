package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultSourceURL = "https://raw.githubusercontent.com/SoliSpirit/mtproto/master/all_proxies.txt"
const defaultRemoteURL = "git@github.com:dzyunrikui/reelsoprovod.git"

type proxy struct {
	Link   string
	Server string
	Port   int
}

func main() {
	sourceURL := flag.String("url", defaultSourceURL, "URL with t.me/proxy links")
	output := flag.String("output", "available_proxies.txt", "file to write available proxy links")
	timeout := flag.Duration("timeout", 3*time.Second, "TCP connection timeout per proxy")
	workers := flag.Int("workers", 80, "number of concurrent checks")
	gitPush := flag.Bool("git-push", false, "commit the output file and push it to git remote")
	remoteURL := flag.String("remote", defaultRemoteURL, "git remote URL used with -git-push")
	commitMessage := flag.String("commit-message", "Update available proxies", "git commit message used with -git-push")
	flag.Parse()

	proxies, err := loadProxies(*sourceURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load proxies: %v\n", err)
		os.Exit(1)
	}

	available := checkProxies(proxies, *timeout, *workers)
	sort.Slice(available, func(i, j int) bool {
		if available[i].Server == available[j].Server {
			if available[i].Port == available[j].Port {
				return available[i].Link < available[j].Link
			}
			return available[i].Port < available[j].Port
		}
		return available[i].Server < available[j].Server
	})

	fmt.Fprintf(os.Stderr, "Checked: %d\n", len(proxies))
	fmt.Fprintf(os.Stderr, "Available TCP: %d\n", len(available))
	if err := writeAvailable(*output, available); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Output: %s\n", *output)

	for _, p := range available {
		fmt.Println(p.Link)
	}

	if *gitPush {
		if err := pushResult(*remoteURL, *output, *commitMessage); err != nil {
			fmt.Fprintf(os.Stderr, "git push result: %v\n", err)
			os.Exit(1)
		}
	}
}

func writeAvailable(path string, available []proxy) error {
	if path == "" {
		return fmt.Errorf("output path is empty")
	}

	var b strings.Builder
	for _, p := range available {
		b.WriteString(p.Link)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func pushResult(remoteURL, outputPath, commitMessage string) error {
	if err := ensureGitRepo(remoteURL); err != nil {
		return err
	}

	if err := runGit("add", "--", outputPath); err != nil {
		return err
	}

	changed, err := hasStagedChanges()
	if err != nil {
		return err
	}
	if changed {
		if err := runGit("commit", "-m", commitMessage); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "No output changes to commit")
	}

	return runGit("push", "-u", "origin", "main")
}

func ensureGitRepo(remoteURL string) error {
	if _, err := os.Stat(".git"); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := runGit("init"); err != nil {
			return err
		}
	}

	if err := ensureOrigin(remoteURL); err != nil {
		return err
	}
	return runGit("branch", "-M", "main")
}

func ensureOrigin(remoteURL string) error {
	if remoteURL == "" {
		return fmt.Errorf("remote URL is empty")
	}

	cmd := exec.Command("git", "remote", "get-url", "origin")
	if err := cmd.Run(); err == nil {
		return runGit("remote", "set-url", "origin", remoteURL)
	}
	return runGit("remote", "add", "origin", remoteURL)
}

func hasStagedChanges() (bool, error) {
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

func runGit(args ...string) error {
	fmt.Fprintf(os.Stderr, "git %s\n", strings.Join(args, " "))
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func loadProxies(sourceURL string) ([]proxy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	var proxies []proxy
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		p, err := parseProxy(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %q: %v\n", line, err)
			continue
		}
		proxies = append(proxies, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return proxies, nil
}

func parseProxy(link string) (proxy, error) {
	u, err := url.Parse(link)
	if err != nil {
		return proxy{}, err
	}

	q := u.Query()
	server := strings.TrimSuffix(q.Get("server"), ".")
	portRaw := q.Get("port")
	if server == "" || portRaw == "" {
		return proxy{}, fmt.Errorf("missing server or port")
	}

	port, err := strconv.Atoi(portRaw)
	if err != nil {
		return proxy{}, fmt.Errorf("invalid port %q: %w", portRaw, err)
	}
	if port <= 0 || port > 65535 {
		return proxy{}, fmt.Errorf("port out of range: %d", port)
	}

	return proxy{Link: link, Server: server, Port: port}, nil
}

func checkProxies(proxies []proxy, timeout time.Duration, workers int) []proxy {
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan proxy)
	results := make(chan proxy)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if isTCPAvailable(p, timeout) {
					results <- p
				}
			}
		}()
	}

	go func() {
		for _, p := range proxies {
			jobs <- p
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var available []proxy
	for p := range results {
		available = append(available, p)
	}
	return available
}

func isTCPAvailable(p proxy, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(p.Server, strconv.Itoa(p.Port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
