// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package mount handles filesystem mount operations.
package mount

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/siderolabs/go-retry/retry"
	"golang.org/x/sys/unix"
)

// Point represents a mount point.
type Point struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
}

// NewPointOption is a mount point option.
type NewPointOption func(*Point)

// WithProjectQuota sets the project quota flag.
func WithProjectQuota(enabled bool) NewPointOption {
	return func(p *Point) {
		if !enabled {
			return
		}

		if len(p.data) > 0 {
			p.data += ","
		}

		p.data += "prjquota"
	}
}

// WithFlags sets the mount flags.
func WithFlags(flags uintptr) NewPointOption {
	return func(p *Point) {
		p.flags |= flags
	}
}

// WithReadonly sets the read-only flag.
func WithReadonly() NewPointOption {
	return WithFlags(unix.MS_RDONLY)
}

// NewPoint creates a new mount point.
func NewPoint(source, target, fstype string, opts ...NewPointOption) *Point {
	p := &Point{
		source: source,
		target: target,
		fstype: fstype,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// PrinterOptions are printer options.
type PrinterOptions struct {
	Printer func(string, ...any)
}

// Printf prints a formatted string (or skips if printer is nil).
func (o PrinterOptions) Printf(format string, args ...any) {
	if o.Printer != nil {
		o.Printer(format, args...)
	}
}

// OperationOptions are mount options.
type OperationOptions struct {
	PrinterOptions

	SkipIfMounted bool

	TargetMode os.FileMode
}

// OperationOption is a mount option.
type OperationOption func(*OperationOptions)

// WithSkipIfMounted sets the skip if mounted flag.
func WithSkipIfMounted() OperationOption {
	return func(o *OperationOptions) {
		o.SkipIfMounted = true
	}
}

// WithMountPrinter sets the printer.
func WithMountPrinter(printer func(string, ...any)) OperationOption {
	return func(o *OperationOptions) {
		o.Printer = printer
	}
}

// UnmountOptions is unmount options.
type UnmountOptions struct {
	PrinterOptions
}

// UnmountOption is an unmount option.
type UnmountOption func(*UnmountOptions)

// WithUnmountPrinter sets the printer.
func WithUnmountPrinter(printer func(string, ...any)) UnmountOption {
	return func(o *UnmountOptions) {
		o.Printer = printer
	}
}

// IsMounted checks if the mount point is mounted by checking the mount on the target.
func (p *Point) IsMounted() (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, err
	}

	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) < 2 {
			continue
		}

		mountpoint := fields[1]

		if mountpoint == p.target {
			return true, nil
		}
	}

	return false, scanner.Err()
}

// Mount the mount point.
//
// Mount returns an unmounter function to unmount the mount point.
func (p *Point) Mount(opts ...OperationOption) (unmounter func() error, err error) {
	options := OperationOptions{
		TargetMode: 0o755,
	}

	for _, opt := range opts {
		opt(&options)
	}

	if options.SkipIfMounted {
		isMounted, err := p.IsMounted()
		if err != nil {
			return nil, err
		}

		// already mounted, return a no-op unmounter
		if isMounted {
			return func() error {
				return nil
			}, nil
		}
	}

	if err = os.MkdirAll(p.target, options.TargetMode); err != nil {
		return nil, fmt.Errorf("error creating mount point directory %s: %w", p.target, err)
	}

	err = p.retry(p.mount, false, options.PrinterOptions)
	if err != nil {
		return nil, fmt.Errorf("error mounting %s: %w", p.source, err)
	}

	return func() error {
		return p.Unmount(WithUnmountPrinter(options.Printer))
	}, nil
}

// Unmount the mount point.
func (p *Point) Unmount(opts ...UnmountOption) error {
	var options UnmountOptions

	for _, opt := range opts {
		opt(&options)
	}

	mounted, err := p.IsMounted()
	if err != nil {
		return err
	}

	if !mounted {
		return nil
	}

	return p.retry(func() error {
		return p.unmount(options.Printer)
	}, true, options.PrinterOptions)
}

func (p *Point) mount() error {
	return unix.Mount(p.source, p.target, p.fstype, p.flags, p.data)
}

func (p *Point) unmount(printer func(string, ...any)) error {
	return SafeUnmount(context.Background(), printer, p.target)
}

//nolint:gocyclo
func (p *Point) retry(f func() error, isUnmount bool, printerOptions PrinterOptions) error {
	return retry.Constant(5*time.Second, retry.WithUnits(50*time.Millisecond)).Retry(func() error {
		if err := f(); err != nil {
			switch err {
			case unix.EBUSY:
				return retry.ExpectedError(err)
			case unix.ENOENT, unix.ENXIO:
				// if udevd triggers BLKRRPART ioctl, partition device entry might disappear temporarily
				return retry.ExpectedError(err)
			case unix.EUCLEAN, unix.EIO:
				if !isUnmount {
					if errRepair := p.repair(printerOptions); errRepair != nil {
						return fmt.Errorf("error repairing: %w", errRepair)
					}
				}

				return retry.ExpectedError(err)
			case unix.EINVAL:
				isMounted, checkErr := p.IsMounted()
				if checkErr != nil {
					return retry.ExpectedError(checkErr)
				}

				if !isMounted && !isUnmount {
					if errRepair := p.repair(printerOptions); errRepair != nil {
						return fmt.Errorf("error repairing: %w", errRepair)
					}

					return retry.ExpectedError(err)
				}

				if !isMounted && isUnmount { // if partition is already unmounted, ignore EINVAL
					return nil
				}

				return err
			default:
				return err
			}
		}

		return nil
	})
}
