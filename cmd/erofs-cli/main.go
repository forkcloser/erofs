// Command erofs-cli walks an EROFS image and prints every entry with its
// type, mode, modification time and extended attributes.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/forkcloser/erofs"
)

func main() {
	var path string

	flag.StringVar(&path, "img", "", "Path to erofs image")
	flag.Parse()

	if err := run(path); err != nil {
		log.Fatal(err)
	}
}

func run(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	img, err := erofs.Open(f)
	if err != nil {
		return err
	}

	fmt.Println("Found valid image...")

	err = fs.WalkDir(img, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error visiting %s: %w", path, err)
		}

		fmt.Printf("visited: %q\n", path)
		fmt.Printf("\tName: %q\n", entry.Name())
		fmt.Printf("\tType: %o\n", entry.Type())

		if entry.IsDir() {
			fmt.Println("\tIs a directory: yes")
		} else {
			fmt.Println("\tIs a directory: no")
		}

		fi, err := entry.Info()
		if err != nil {
			return fmt.Errorf("error getting info for %s: %w", path, err)
		}

		fmt.Printf("\tMode: %o\n", fi.Mode())
		fmt.Printf("\tModTime: %s\n", fi.ModTime())

		if st, ok := fi.Sys().(*erofs.Stat); ok && len(st.Xattrs) > 0 {
			fmt.Println("\tXattrs:")

			for k, v := range st.Xattrs {
				fmt.Printf("\t\t%s: %q\n", k, v)
			}
		}

		return nil
	})

	return err
}
