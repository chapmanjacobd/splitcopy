# splitcopy

A small Go-based utility for copying and merging directories. It is designed to handle interruptions gracefully, ensuring progress is saved even if the process is stopped by a user or due to insufficient disk space.

Interruptible Progress: If stopped via Ctrl+C or ENOSPC, the script completes its filesystem scan and saves a list of remaining files to [sourceDir].remainingfiles. The next time you run the script you can ensure you are only copying files which haven't been copied before. This is uesful for splitting up a large read-only disk into multiple smaller disks.
