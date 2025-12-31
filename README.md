# splitcopy

A small CLI utility for copying and merging directories. It is designed to handle interruptions gracefully, ensuring progress is saved even if the process is stopped by a user or due to insufficient disk space.

Interruptible Progress: If stopped via Ctrl+C or ENOSPC, the script completes its filesystem scan and saves a list of remaining files to [sourceDir].remainingfiles. The next time you run the script you can ensure you are only copying files which haven't been copied before. This is uesful for splitting up a large read-only disk into multiple smaller disks.

Note: it will not copy empty folders

## Install

    go install github.com/chapmanjacobd/splitcopy@latest

## Usage

    $ splitcopy /src/folder/ /dest/folder/
    ^C
    Interrupt received. Finishing source directory tree scan...
    Press Ctrl+C again in >2s to cancel and delete incomplete progress file

    Remaining paths saved to: folder.remainingfiles
    $ splitcopy /src/folder/ /dest/folder/ --resume=folder.remainingfiles
    (repeat as many times as desired or wait to hit ENOSPC error)

## Help

    $ splitcopy -h
    Usage: splitcopy <source> <destination> [flags]

    Arguments:
    <source>         Source directory.
    <destination>    Destination directory.

    Flags:
    -h, --help           Show context-sensitive help.
    -r, --resume=FILE    Text file containing relative paths to copy.
