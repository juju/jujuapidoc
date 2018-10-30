// The jujuapidoc command generates a JSON file containing
// details of as many Juju RPC calls as it can get its hands on.
//
// It depends on a custom addition to the apiserver package,
// FacadeRegistry.ListDetails, the implementation of which
// can be found in https://github.com/juju/juju/tree/076-apiserver-facade-list-details.
//
// The resulting JSON output can be processed into HTML by
// the jujuapidochtml command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/errgo.v1"
)

//go:generate go-bindata jujugenerateapidoc

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: jujuapidoc [juju-version]\n")
		os.Exit(2)
	}
	flag.Parse()
	version := flag.Arg(0)
	if version == "" {
		version = "latest"
	}
	if !canUseModules() {
		fmt.Fprintf(os.Stderr, "cannot use Go modules; use Go 1.11 or later\n")
		os.Exit(1)
	}
	if err := runMain(version); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func canUseModules() bool {
	_, err := runCmd("", "go", "help", "mod")
	return err == nil
}

func runMain(version string) error {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return errgo.Mask(err)
	}
	log.Printf("temp dir: %v", dir)
	//defer os.RemoveAll(dir)
	jujuModDir := filepath.Join(dir, "jujumod")
	if err := os.Mkdir(jujuModDir, 0777); err != nil {
		return errgo.Mask(err)
	}

	if err := RestoreAssets(dir, ""); err != nil {
		return errgo.Mask(err)
	}
	generateDir := filepath.Join(dir, "jujugenerateapidoc")

	jujuDir, err := runCmd(generateDir, "go", "list", "-f={{.Dir}}", "-m", "github.com/juju/juju@"+version)
	if err != nil {
		return errgo.Mask(err)
	}
	jujuDir = strings.TrimSpace(jujuDir)
	if err := copyFile(filepath.Join(jujuModDir, "Gopkg.lock"), filepath.Join(jujuDir, "Gopkg.lock")); err != nil {
		return errgo.Mask(err)
	}
	if err := copyFile(filepath.Join(jujuModDir, "Gopkg.toml"), filepath.Join(jujuDir, "Gopkg.toml")); err != nil {
		return errgo.Mask(err)
	}
	if _, err := runCmd(jujuModDir, "go", "mod", "init", "github.com/juju/juju"); err != nil {
		return errgo.Mask(err)
	}
	if _, err := runCmd(generateDir, "gomodmerge", filepath.Join(jujuModDir, "go.mod")); err != nil {
		return errgo.Notef(err, `cannot run gomodmerge; try "go get github.com/rogpeppe/gomodmerge"`)
	}
	if _, err := runCmd(generateDir, "go", "build"); err != nil {
		return errgo.Notef(err, "cannot build doc generator program")
	}
	cmd := exec.Command(filepath.Join(generateDir, "jujugenerateapidoc"))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return errgo.Notef(err, "generate info failed")
	}
	return nil
}

func runCmd(dir string, exe string, args ...string) (string, error) {
	c := exec.Command(exe, args...)
	c.Stderr = os.Stderr
	c.Dir = dir
	var buf bytes.Buffer
	c.Stdout = &buf
	if err := c.Run(); err != nil {
		return "", errgo.Notef(err, "cannot run %s %q in dir %q", exe, args, dir)
	}
	return buf.String(), nil
}

func copyFile(dst, src string) error {
	data, err := ioutil.ReadFile(src)
	if err != nil {
		return errgo.Notef(err, "cannot read file")
	}
	if err := ioutil.WriteFile(dst, data, 0666); err != nil {
		return errgo.Notef(err, "cannot write file")
	}
	return nil
}
