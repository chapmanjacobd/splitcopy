package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
)

type CLI struct {
	InputDir  string `arg:"" name:"input" help:"Input directory to copy from" type:"path"`
	OutputDir string `arg:"" name:"output" help:"Output directory to merge into" type:"path"`
	FileList  string `arg:"" optional:"" name:"filelist" help:"Text file with relative paths to copy" type:"path"`
}

type CopyManager struct {
	inputDir       string
	outputDir      string
	remainingFiles []string
	interrupted    bool
	outOfSpace     bool
	currentFile    string
}

func main() {
	var cli CLI
	kong.Parse(&cli)

	manager := &CopyManager{
		inputDir:  cli.InputDir,
		outputDir: cli.OutputDir,
	}

	// Setup signal handler for Ctrl-C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		manager.interrupted = true
		fmt.Println("\n\nInterrupted by user (Ctrl-C)")
	}()

	// Get list of files to copy
	var err error
	if cli.FileList != "" {
		manager.remainingFiles, err = loadFileList(cli.FileList)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading file list: %v\n", err)
			os.Exit(1)
		}
	} else {
		manager.remainingFiles, err = scanDirectory(cli.InputDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning directory: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("Found %d files to copy\n", len(manager.remainingFiles))

	// Copy files
	manager.copyFiles()

	// Handle interruption or out of space
	if manager.interrupted || manager.outOfSpace {
		if err := manager.saveRemainingFiles(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving remaining files: %v\n", err)
			os.Exit(1)
		}

		if manager.outOfSpace {
			manager.promptForNewDisk()
		} else {
			fmt.Println("Exiting due to user interruption")
			os.Exit(0)
		}
	} else {
		fmt.Println("All files copied successfully!")
	}
}

func scanDirectory(rootDir string) ([]string, error) {
	var files []string
	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(rootDir, path)
			if err != nil {
				return err
			}
			files = append(files, relPath)
		}
		return nil
	})
	return files, err
}

func loadFileList(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var files []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			files = append(files, line)
		}
	}
	return files, scanner.Err()
}

func (m *CopyManager) copyFiles() {
	for i := 0; i < len(m.remainingFiles); i++ {
		if m.interrupted {
			m.remainingFiles = m.remainingFiles[i:]
			return
		}

		relPath := m.remainingFiles[i]
		m.currentFile = relPath
		srcPath := filepath.Join(m.inputDir, relPath)
		dstPath := filepath.Join(m.outputDir, relPath)

		fmt.Printf("Copying: %s\n", relPath)

		if err := m.copyFile(srcPath, dstPath); err != nil {
			if isOutOfSpaceError(err) {
				m.outOfSpace = true
				m.remainingFiles = m.remainingFiles[i:]
				fmt.Printf("\nOutput directory is out of space!\n")
				// Delete partial file
				os.Remove(dstPath)

				// Continue scanning to get full list
				if i < len(m.remainingFiles)-1 {
					fmt.Println("Continuing to scan remaining files...")
				}
				return
			}
			fmt.Fprintf(os.Stderr, "Error copying %s: %v\n", relPath, err)
		}
	}
	m.remainingFiles = nil
}

func (m *CopyManager) copyFile(src, dst string) error {
	// Get source file info
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}

	// Create destination directory
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}

	// Handle symlinks
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return copySymlink(src, dst)
	}

	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Create destination file
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Copy with periodic interrupt checking
	buf := make([]byte, 1024*1024) // 1MB buffer
	for {
		if m.interrupted {
			dstFile.Close()
			os.Remove(dst)
			return fmt.Errorf("interrupted")
		}

		n, err := srcFile.Read(buf)
		if n > 0 {
			if _, writeErr := dstFile.Write(buf[:n]); writeErr != nil {
				dstFile.Close()
				os.Remove(dst)
				return writeErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			dstFile.Close()
			os.Remove(dst)
			return err
		}
	}

	// Close the file before setting metadata
	dstFile.Close()

	// Preserve permissions
	if err := os.Chmod(dst, srcInfo.Mode()); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Preserve modification and access times
	atime := getAtime(srcInfo)
	mtime := srcInfo.ModTime()
	if err := os.Chtimes(dst, atime, mtime); err != nil {
		return fmt.Errorf("failed to set times: %w", err)
	}

	// Preserve ownership (may require root privileges)
	if err := preserveOwnership(src, dst, srcInfo); err != nil {
		// Don't fail on ownership errors, just warn
		fmt.Fprintf(os.Stderr, "Warning: failed to preserve ownership for %s: %v\n", dst, err)
	}

	return nil
}

func copySymlink(src, dst string) error {
	// Read the symlink target
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}

	// Remove destination if it exists
	os.Remove(dst)

	// Create the symlink
	if err := os.Symlink(target, dst); err != nil {
		return err
	}

	// Get source symlink info for ownership
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}

	// Preserve ownership on the symlink itself
	if err := preserveOwnership(src, dst, srcInfo); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to preserve symlink ownership for %s: %v\n", dst, err)
	}

	return nil
}

func getAtime(info os.FileInfo) time.Time {
	// Try to get access time from system-specific stat
	if sys := info.Sys(); sys != nil {
		if stat, ok := sys.(*syscall.Stat_t); ok {
			return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
		}
	}
	// Fallback to modification time if access time unavailable
	return info.ModTime()
}

func preserveOwnership(src, dst string, srcInfo os.FileInfo) error {
	// Get the underlying syscall.Stat_t
	if sys := srcInfo.Sys(); sys != nil {
		if stat, ok := sys.(*syscall.Stat_t); ok {
			uid := int(stat.Uid)
			gid := int(stat.Gid)

			// Use Lchown to change ownership without following symlinks
			return os.Lchown(dst, uid, gid)
		}
	}
	return fmt.Errorf("could not get ownership information")
}

func (m *CopyManager) saveRemainingFiles() error {
	if len(m.remainingFiles) == 0 {
		return nil
	}

	baseName := filepath.Base(m.inputDir)
	fileName := fmt.Sprintf("%s.remainingfiles", baseName)

	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, relPath := range m.remainingFiles {
		fmt.Fprintln(writer, relPath)
	}
	writer.Flush()

	fmt.Printf("\nRemaining files saved to: %s\n", fileName)
	fmt.Printf("Files remaining: %d\n", len(m.remainingFiles))
	return nil
}

func (m *CopyManager) promptForNewDisk() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nInsert disk 2 and enter the new output path (or 'q' to quit): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "q" || input == "Q" {
			fmt.Println("Exiting...")
			os.Exit(0)
		}

		if input != "" {
			m.outputDir = input
			m.outOfSpace = false
			m.copyFiles()

			if !m.outOfSpace && !m.interrupted {
				fmt.Println("All remaining files copied successfully!")
				// Clean up the remaining files list
				baseName := filepath.Base(m.inputDir)
				fileName := fmt.Sprintf("%s.remainingfiles", baseName)
				os.Remove(fileName)
				os.Exit(0)
			}
		}
	}
}

func isOutOfSpaceError(err error) bool {
	if err == nil {
		return false
	}
	// Check for "no space left on device" error
	return strings.Contains(err.Error(), "no space left") ||
		strings.Contains(err.Error(), "disk full") ||
		strings.Contains(strings.ToLower(err.Error()), "enospc")
}
