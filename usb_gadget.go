package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	gadgetName = "tsdisk"
	gadgetPath = "/sys/kernel/config/usb_gadget/" + gadgetName
	udcPath    = "/sys/class/udc"
)

// USBGadget manages the USB mass-storage gadget via configfs.
type USBGadget struct {
	backingDev string
}

func NewUSBGadget(backingDev string) *USBGadget {
	return &USBGadget{backingDev: backingDev}
}

// Setup creates the gadget, configures mass_storage, and binds it to a UDC.
func (g *USBGadget) Setup() error {
	// Clean up any existing gadget.
	g.Teardown()

	// Create gadget directory.
	if err := os.MkdirAll(gadgetPath, 0755); err != nil {
		return fmt.Errorf("create gadget dir: %w", err)
	}

	// Set USB IDs (generic mass storage).
	g.writeFile(filepath.Join(gadgetPath, "idVendor"), "0x1d6b")
	g.writeFile(filepath.Join(gadgetPath, "idProduct"), "0x0104")
	g.writeFile(filepath.Join(gadgetPath, "bcdDevice"), "0x0100")
	g.writeFile(filepath.Join(gadgetPath, "bcdUSB"), "0x0200")

	// Strings.
	os.MkdirAll(filepath.Join(gadgetPath, "strings/0x409"), 0755)
	g.writeFile(filepath.Join(gadgetPath, "strings/0x409/serialnumber"), "9911010101")
	g.writeFile(filepath.Join(gadgetPath, "strings/0x409/manufacturer"), "yonleLABORATORY")
	g.writeFile(filepath.Join(gadgetPath, "strings/0x409/product"), "TSSniff")

	// Config.
	cfgDir := filepath.Join(gadgetPath, "configs/c.1")
	os.MkdirAll(filepath.Join(cfgDir, "strings/0x409"), 0755)
	g.writeFile(filepath.Join(cfgDir, "strings/0x409/configuration"), "Mass Storage")
	g.writeFile(filepath.Join(cfgDir, "MaxPower"), "250")

	// Function: mass_storage.
	funcDir := filepath.Join(gadgetPath, "functions/mass_storage.0")
	if err := os.MkdirAll(funcDir, 0755); err != nil {
		return fmt.Errorf("create mass_storage function: %w", err)
	}
	g.writeFile(filepath.Join(funcDir, "stall"), "1")

	// LUN 0: backing block device.
	lunDir := filepath.Join(funcDir, "lun.0")
	os.MkdirAll(lunDir, 0755)
	g.writeFile(filepath.Join(lunDir, "cdrom"), "0")
	g.writeFile(filepath.Join(lunDir, "ro"), "0")
	g.writeFile(filepath.Join(lunDir, "removable"), "1")
	g.writeFile(filepath.Join(lunDir, "file"), g.backingDev)

	// Link function to config.
	linkPath := filepath.Join(cfgDir, "mass_storage.0")
	os.Symlink(funcDir, linkPath)

	// Bind to UDC.
	udc, err := g.findUDC()
	if err != nil {
		return fmt.Errorf("find UDC: %w", err)
	}
	g.writeFile(filepath.Join(gadgetPath, "UDC"), udc)

	log.Printf("USB gadget bound to %s with backing %s", udc, g.backingDev)
	return nil
}

// Teardown removes the gadget.
func (g *USBGadget) Teardown() {
	// Unbind UDC.
	g.writeFile(filepath.Join(gadgetPath, "UDC"), "")

	// Remove symlink.
	os.Remove(filepath.Join(gadgetPath, "configs/c.1/mass_storage.0"))

	// Remove function.
	os.RemoveAll(filepath.Join(gadgetPath, "functions/mass_storage.0"))

	// Remove config.
	os.RemoveAll(filepath.Join(gadgetPath, "configs/c.1"))

	// Remove strings.
	os.RemoveAll(filepath.Join(gadgetPath, "strings/0x409"))

	// Remove gadget.
	os.RemoveAll(gadgetPath)
}

func (g *USBGadget) findUDC() (string, error) {
	entries, err := os.ReadDir(udcPath)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		return e.Name(), nil
	}
	return "", fmt.Errorf("no UDC found")
}

func (g *USBGadget) writeFile(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		log.Printf("Warning: write %s: %v", path, err)
	}
}

// WaitForNBDDevice waits for the NBD device node to appear.
func WaitForNBDDevice(devPath string) error {
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(devPath); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", devPath)
}
