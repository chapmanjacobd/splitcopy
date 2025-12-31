package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/ergochat/readline"
)

type CLI struct {
	Source      string   `arg:"" help:"Source directory." type:"existingdir"`
	Destination string   `arg:"" help:"Destination directory." type:"path"`
	ResumeList  *os.File `name:"resume" short:"r" placeholder:"FILE" help:"Text file containing relative paths to copy."`
}

type Session struct {
	args     *CLI
	sigChan  chan os.Signal
	mu       sync.Mutex
	cond     *sync.Cond
	allPaths []string
	scanDone bool
	scanErr  error
}

func main() {
	var args CLI
	kong.Parse(&args)

	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	sess := &Session{
		args:    &args,
		sigChan: sigChan,
	}
	sess.cond = sync.NewCond(&sess.mu)

	if err := sess.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func (s *Session) Run() error {
	go s.scan()
	return s.copyLoop(0)
}

func (s *Session) scan() {
	defer func() {
		s.mu.Lock()
		s.scanDone = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}()

	if s.args.ResumeList != nil {
		defer s.args.ResumeList.Close()
		scanner := bufio.NewScanner(s.args.ResumeList)
		for scanner.Scan() {
			s.addPath(scanner.Text())
		}
		s.scanErr = scanner.Err()
	} else {
		s.scanErr = filepath.WalkDir(s.args.Source, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(s.args.Source, path)
			s.addPath(rel)
			return nil
		})
	}
}

func (s *Session) addPath(rel string) {
	s.mu.Lock()
	s.allPaths = append(s.allPaths, rel)
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *Session) copyLoop(startIndex int) error {
	currentIndex := startIndex

	for {
		select {
		case <-s.sigChan:
			return s.handleInterrupt(currentIndex, true)
		default:
			s.mu.Lock()
			for currentIndex >= len(s.allPaths) && !s.scanDone {
				s.mu.Unlock()
				select {
				case <-s.sigChan:
					return s.handleInterrupt(currentIndex, true)
				case <-time.After(50 * time.Millisecond):
					s.mu.Lock()
				}
			}

			if currentIndex >= len(s.allPaths) && s.scanDone {
				err := s.scanErr
				s.mu.Unlock()
				return err
			}
			relPath := s.allPaths[currentIndex]
			s.mu.Unlock()

			src := filepath.Join(s.args.Source, relPath)
			dst := filepath.Join(s.args.Destination, relPath)

			if err := s.copyFile(src, dst); err != nil {
				if errors.Is(err, syscall.ENOSPC) {
					fmt.Printf("\nDisk full: %s\n", relPath)
					s.handleInterrupt(currentIndex, false)

					newDest, err := s.promptForNewPath()
					if err != nil {
						return err
					}
					s.args.Destination = newDest
					return s.copyLoop(currentIndex)
				}
				return err
			}
			currentIndex++
		}
	}
}

func (s *Session) handleInterrupt(startIdx int, isUserQuit bool) error {
	if isUserQuit {
		interruptTime := time.Now()
		fmt.Println("\nInterrupt received. Finishing source directory tree scan...")
		fmt.Println("Press Ctrl+C again in >2s to cancel and delete incomplete progress file")
		fmt.Println()

		go func() {
			<-s.sigChan
			if time.Since(interruptTime) > 2*time.Second {
				os.Remove(filepath.Base(s.args.Source) + ".remainingfiles")
				fmt.Println("\nCancelled. Progress file deleted.")
				os.Exit(1)
			}
		}()
	}

	s.mu.Lock()
	for !s.scanDone {
		s.cond.Wait()
	}
	remaining := s.allPaths[startIdx:]
	s.mu.Unlock()

	s.saveRemaining(remaining)
	if isUserQuit {
		os.Exit(0)
	}
	return nil
}

func (s *Session) promptForNewPath() (string, error) {
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
		os.Remove(dst)
		return err
	}

	os.Chtimes(dst, sInfo.ModTime(), sInfo.ModTime())
	if stat, ok := sInfo.Sys().(*syscall.Stat_t); ok {
		os.Chown(dst, int(stat.Uid), int(stat.Gid))
	}
	return nil
}

func (s *Session) saveRemaining(remaining []string) {
	if len(remaining) == 0 {
		return
	}
	name := filepath.Base(s.args.Source) + ".remainingfiles"
	f, _ := os.Create(name)
	defer f.Close()
	for _, line := range remaining {
		fmt.Fprintln(f, line)
	}
	fmt.Printf("Remaining paths saved to: %s\n", name)
}
