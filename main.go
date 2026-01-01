package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/ergochat/readline"
	"golang.org/x/term"
)

type CLI struct {
	Source      string   `arg:"" help:"Source directory." type:"existingdir"`
	Destination string   `arg:"" help:"Destination directory." type:"path"`
	ResumeList  *os.File `name:"resume" short:"r" placeholder:"FILE" help:"Text file containing relative paths to copy."`
}

func main() {
	var args CLI
	kong.Parse(&args)

	sigIntChan := make(chan os.Signal, 1)
	signal.Notify(sigIntChan, os.Interrupt, syscall.SIGTERM)

	sess := &Session{
		args:       &args,
		sigIntChan: sigIntChan,
		progress: Progress{
			start: time.Now(),
		},
	}

	sess.watchResize()

	if err := sess.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func (s *Session) scan(paths chan<- string, errCh chan<- error) {
	defer close(paths)

	if s.args.ResumeList != nil {
		defer s.args.ResumeList.Close()
		scanner := bufio.NewScanner(s.args.ResumeList)
		for scanner.Scan() {
			paths <- scanner.Text()
		}
		errCh <- scanner.Err()
		return
	}

	errCh <- filepath.WalkDir(s.args.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(s.args.Source, path)
		paths <- rel
		return nil
	})
}

type Stats struct {
	Files int64
	Bytes int64
}

type Progress struct {
	Global        Stats
	Local         Stats
	start         time.Time
	lastPrintTime time.Time
}

type Session struct {
	args       *CLI
	sigIntChan chan os.Signal

	termWidth  int
	progress   Progress
	currentRel string
}

func (s *Session) Run() error {
	paths := make(chan string)
	errCh := make(chan error, 1)

	go s.scan(paths, errCh)

	for {
		select {

		case <-s.sigIntChan:
			return s.exitWithRemaining(paths)

		case rel, more := <-paths:
			if !more {
				s.printProgress()
				fmt.Println()
				return <-errCh
			}

			if err := s.copyWithRetry(rel, paths); err != nil {
				return err
			}
		}
	}
}

func (s *Session) copyWithRetry(rel string, paths <-chan string) error {
	s.currentRel = rel

	src := filepath.Join(s.args.Source, rel)
	sInfo, err := os.Stat(src)
	if err != nil {
		fmt.Println()
		fmt.Printf("Stat error %v: %s\n", err, rel)
		return nil
	}

	size := sInfo.Size()

	if time.Since(s.progress.lastPrintTime) >= 320*time.Millisecond {
		s.printProgress()
	}

	for {
		// Check for interrupt
		if len(s.sigIntChan) > 0 {
			return s.exitWithRemaining(paths)
		}

		dst := filepath.Join(s.args.Destination, rel)
		err := s.copyFile(src, dst)
		if err == nil {
			s.progress.Global.Files++
			s.progress.Global.Bytes += size
			s.progress.Local.Files++
			s.progress.Local.Bytes += size

			s.currentRel = ""
			return nil
		} else {
			fmt.Println()
			fmt.Printf("%v\n", err)

			newDest, err := s.promptForNewPath()
			if err != nil {
				return s.exitWithRemaining(paths)
			}
			s.args.Destination = newDest
			// Reset local stats for new destination
			s.progress.Local = Stats{}
			s.progress.start = time.Now()
		}
	}
}

func (s *Session) copyFile(src, dst string) error {
	sInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	fSrc, err := os.Open(src)
	if err != nil {
		return err
	}
	defer fSrc.Close()

	fDst, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, err = io.Copy(fDst, fSrc)
	fDst.Close()

	if err != nil {
		_ = os.Remove(dst)
		return err
	}

	os.Chtimes(dst, sInfo.ModTime(), sInfo.ModTime())
	if stat, ok := sInfo.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(dst, int(stat.Uid), int(stat.Gid))
	}
	return nil
}

func (s *Session) printProgress() {
	elapsed := time.Since(s.progress.start).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(s.progress.Local.Bytes) / elapsed
	}

	status := fmt.Sprintf("\r[Global: %d files, %s]%s | %s/s",
		s.progress.Global.Files,
		humanBytes(s.progress.Global.Bytes),
		func() string {
			if s.progress.Global.Files == s.progress.Local.Files {
				return ""
			}
			return fmt.Sprintf(" [Dest: %d files, %s]", s.progress.Local.Files, humanBytes(s.progress.Local.Bytes))
		}(),
		humanBytes(int64(rate)),
	)

	remainingSpace := s.termWidth - len(status) - 4
	if remainingSpace > 10 {
		status = status + " | " + truncateMiddle(s.currentRel, remainingSpace) + "\033[K"
	} else {
		status = status + "\033[K"
	}

	fmt.Print(status)
	s.progress.lastPrintTime = time.Now()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= unit*div {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncateMiddle(s string, max int) string {
	if s == "" {
		return ""
	}
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + "…" + s[len(s)-half:]
}

func (s *Session) promptForNewPath() (string, error) {
	fmt.Println()
	fmt.Printf("Enter new destination path (ie. \"Disk 2\"):\n")

	rl, err := readline.NewEx(&readline.Config{
		Prompt: "",
		AutoComplete: readline.NewPrefixCompleter(
			readline.PcItemDynamic(func(line string) []string {
				dir := filepath.Dir(line)
				if dir == "" {
					dir = "."
				}
				entries, _ := os.ReadDir(dir)
				var results []string
				for _, e := range entries {
					if e.IsDir() {
						results = append(results, filepath.Join(dir, e.Name()))
					}
				}
				return results
			}),
		),
	})
	if err != nil {
		return "", err
	}
	defer rl.Close()

	input, err := rl.ReadLineWithDefault(s.args.Destination)
	return strings.TrimSpace(input), err
}

func (s *Session) exitWithRemaining(paths <-chan string) error {
	if s.args.ResumeList == nil {
		fmt.Println("\nInterrupt received. Finishing source directory tree scan...")
	}

	var remaining []string
	if s.currentRel != "" {
		remaining = append(remaining, s.currentRel)
	}
	for rel := range paths {
		remaining = append(remaining, rel)
	}

	s.saveRemaining(remaining)
	os.Exit(130)
	return nil
}

func (s *Session) saveRemaining(remaining []string) {
	if len(remaining) == 0 {
		return
	}

	name := filepath.Base(s.args.Source) + ".remainingfiles"
	f, err := os.Create(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to write remaining file: %v\n", err)
		return
	}
	defer f.Close()

	for _, line := range remaining {
		fmt.Fprintln(f, line)
	}
	fmt.Println()
	fmt.Printf("Remaining paths saved to: %s\n", name)
}

func (s *Session) watchResize() {
	// Initialize width
	s.updateWidth()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	go func() {
		for range sigChan {
			s.updateWidth()
		}
	}()
}

func (s *Session) updateWidth() {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		s.termWidth = 80 // Fallback
		return
	}
	s.termWidth = w
}
