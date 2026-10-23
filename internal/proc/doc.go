// Package proc holds the platform-specific system calls sam uses to observe a
// child process's exit without reaping it and to probe whether its process
// group still has members. Linux and macOS are supported.
package proc
