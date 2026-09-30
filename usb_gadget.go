package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const (
	gadgetName = "tsdisk"

	gadgetPath = "/sys/kernel/config/usb_gadget/" +
		gadgetName

	udcPath = "/sys/class/udc"
)

type USBGadget struct {
	backingDev string
}

func NewUSBGadget(
	backingDev string,
) *USBGadget {
	return &USBGadget{
		backingDev: backingDev,
	}
}

func (g *USBGadget) Setup() error {
	g.Teardown()

	if err := os.MkdirAll(
		gadgetPath,
		0o755,
	); err != nil {
		return fmt.Errorf(
			"create gadget dir: %w",
			err,
		)
	}

	g.writeFile(
		filepath.Join(
			gadgetPath,
			"idVendor",
		),
		"0x1d6b",
	)

	g.writeFile(
		filepath.Join(
			gadgetPath,
			"idProduct",
		),
		"0x0104",
	)

	g.writeFile(
		filepath.Join(
			gadgetPath,
			"bcdDevice",
		),
		"0x0100",
	)

	g.writeFile(
		filepath.Join(
			gadgetPath,
			"bcdUSB",
		),
		"0x0200",
	)

	stringsDir := filepath.Join(
		gadgetPath,
		"strings/0x409",
	)

	if err := os.MkdirAll(
		stringsDir,
		0o755,
	); err != nil {
		return fmt.Errorf(
			"create gadget strings: %w",
			err,
		)
	}

	g.writeFile(
		filepath.Join(
			stringsDir,
			"serialnumber",
		),
		"9911010101",
	)

	g.writeFile(
		filepath.Join(
			stringsDir,
			"manufacturer",
		),
		"yonleLABORATORY",
	)

	g.writeFile(
		filepath.Join(
			stringsDir,
			"product",
		),
		"TSSniff",
	)

	cfgDir := filepath.Join(
		gadgetPath,
		"configs/c.1",
	)

	cfgStrings := filepath.Join(
		cfgDir,
		"strings/0x409",
	)

	if err := os.MkdirAll(
		cfgStrings,
		0o755,
	); err != nil {
		return fmt.Errorf(
			"create config strings: %w",
			err,
		)
	}

	g.writeFile(
		filepath.Join(
			cfgStrings,
			"configuration",
		),
		"Mass Storage",
	)

	g.writeFile(
		filepath.Join(
			cfgDir,
			"MaxPower",
		),
		"250",
	)

	funcDir := filepath.Join(
		gadgetPath,
		"functions/mass_storage.0",
	)

	if err := os.MkdirAll(
		funcDir,
		0o755,
	); err != nil {
		return fmt.Errorf(
			"create mass storage function: %w",
			err,
		)
	}

	g.writeFile(
		filepath.Join(
			funcDir,
			"stall",
		),
		"0",
	)

	g.writeFile(
		filepath.Join(
			funcDir,
			"num_buffers",
		),
		"8",
	)

	lunDir := filepath.Join(
		funcDir,
		"lun.0",
	)

	if err := os.MkdirAll(
		lunDir,
		0o755,
	); err != nil {
		return fmt.Errorf(
			"create LUN: %w",
			err,
		)
	}

	/*
		Keep the current behavior from your working tree.
	*/
	g.writeFile(
		filepath.Join(
			lunDir,
			"nofua",
		),
		"0",
	)

	g.writeFile(
		filepath.Join(
			lunDir,
			"cdrom",
		),
		"0",
	)

	g.writeFile(
		filepath.Join(
			lunDir,
			"ro",
		),
		"0",
	)

	g.writeFile(
		filepath.Join(
			lunDir,
			"removable",
		),
		"1",
	)

	g.writeFile(
		filepath.Join(
			lunDir,
			"file",
		),
		g.backingDev,
	)

	g.writeFile(
		filepath.Join(
			lunDir,
			"inquiry_string",
		),
		"TSSniff Disk",
	)

	linkPath := filepath.Join(
		cfgDir,
		"mass_storage.0",
	)

	if err := os.Symlink(
		funcDir,
		linkPath,
	); err != nil {
		return fmt.Errorf(
			"link mass storage function: %w",
			err,
		)
	}

	udc, err := g.findUDC()
	if err != nil {
		return fmt.Errorf(
			"find UDC: %w",
			err,
		)
	}

	g.writeFile(
		filepath.Join(
			gadgetPath,
			"UDC",
		),
		udc,
	)

	log.Printf(
		"USB gadget bound to %s with backing %s",
		udc,
		g.backingDev,
	)

	return nil
}

func (g *USBGadget) Teardown() {
	if _, err := os.Stat(gadgetPath); os.IsNotExist(err) {
		return
	}

	udcFile := filepath.Join(
		gadgetPath,
		"UDC",
	)

	if _, err := os.Stat(udcFile); err == nil {
		g.writeFile(
			udcFile,
			"",
		)
	}

	_ = os.Remove(
		filepath.Join(
			gadgetPath,
			"configs/c.1/mass_storage.0",
		),
	)

	_ = os.RemoveAll(
		filepath.Join(
			gadgetPath,
			"functions/mass_storage.0",
		),
	)

	_ = os.RemoveAll(
		filepath.Join(
			gadgetPath,
			"configs/c.1",
		),
	)

	_ = os.RemoveAll(
		filepath.Join(
			gadgetPath,
			"strings/0x409",
		),
	)

	_ = os.RemoveAll(
		gadgetPath,
	)
}

func (g *USBGadget) findUDC() (string, error) {
	entries, err := os.ReadDir(
		udcPath,
	)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		return entry.Name(), nil
	}

	return "",
		fmt.Errorf(
			"no UDC found",
		)
}

func (g *USBGadget) writeFile(
	path,
	value string,
) {
	if err := os.WriteFile(
		path,
		[]byte(value),
		0o644,
	); err != nil {

		if os.IsNotExist(err) {
			return
		}

		log.Printf(
			"warning: write %s: %v",
			path,
			err,
		)
	}
}
