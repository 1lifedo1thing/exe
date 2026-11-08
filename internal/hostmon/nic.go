package hostmon

import (
	"fmt"
	"regexp"
	"strings"
)

// A network interface's model is its controller's maker and chip, "Intel
// I219-LM" or "Realtek RTL8127": the maker from the PCI vendor ID, the chip
// from the name the system gives the device (pci.ids on Linux, the driver's
// IOModel on a Mac). Each reader looks it up once a device.

// nicVendors are the network makers by PCI vendor ID, by the names their
// chips are known by (pci.ids spells some in capitals, or in full).
var nicVendors = map[uint16]string{
	0x8086: "Intel", 0x10ec: "Realtek", 0x14c3: "MediaTek", 0x1d6a: "Aquantia", 0x15b3: "Mellanox",
	0x14e4: "Broadcom", 0x168c: "Qualcomm Atheros", 0x1969: "Qualcomm Atheros", 0x17cb: "Qualcomm",
	0x11ab: "Marvell", 0x1b4b: "Marvell", 0x10de: "NVIDIA", 0x1077: "QLogic", 0x1924: "Solarflare",
	0x19a2: "Emulex", 0x1fc9: "Tehuti", 0x8088: "Wangxun", 0x1137: "Cisco", 0x177d: "Cavium",
	0x1af4: "Red Hat", 0x15ad: "VMware", 0x1414: "Microsoft", 0x1d0f: "Amazon", 0x1ae0: "Google", 0x106b: "Apple",
}

// nicModel names a PCI network controller: vendorName is the system's name
// for its maker where the table has none, device the system's name for it.
func nicModel(vendor uint16, vendorName, device string) string {
	v := nicVendors[vendor]
	if v == "" {
		v = shortVendor(vendorName)
	}
	m := chipName(device)
	if m == "" {
		m = strings.TrimSpace(device)
	}
	return joinModel(v, m)
}

// usbModel names a USB adapter by what it says of itself.
func usbModel(manufacturer, product string) string {
	m := chipName(product)
	if m == "" {
		m = strings.TrimSpace(product)
	}
	return joinModel(shortVendor(manufacturer), m)
}

func joinModel(vendor, model string) string {
	switch {
	case model == "":
		return vendor
	case vendor == "" || strings.HasPrefix(strings.ToLower(model), strings.ToLower(vendor)):
		return model
	}
	return vendor + " " + model
}

// shortVendor drops what follows a maker's name: "Realtek Semiconductor
// Co., Ltd." is Realtek, "ASIX Electronics Corp." ASIX.
func shortVendor(s string) string {
	for _, cut := range []string{",", " Corporation", " Corp", " Inc", " Co.", " Ltd", " Limited", " Technologies",
		" Technology", " Semiconductor", " Electronics", " Communications", " and subsidiaries", " [", " ("} {
		if i := strings.Index(s, cut); i > 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// chipToken is a part number: capitals, digits and hyphens, with three
// digits at least — I219-LM, AQC113, RTL8125, X710, 82574L, BCM57416 —
// and perhaps a small letter after (Intel's AX1650s, AX1675x); not a
// speed (10GbE, 2.5G, 160MHz) or a standard (802.11ax).
var chipToken = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]*[a-z]?$`)
var unit = regexp.MustCompile(`[0-9]([KMG]Hz|[MG]b)$`)

// chipName finds the chip in a device's name: "Ethernet Connection (17)
// I219-LM" is I219-LM, "RTL8111/8168/8211/8411 PCI Express Gigabit
// Ethernet Controller" RTL8111, and a Mellanox card goes by its family,
// "MT2910 Family [ConnectX-7]". It is "" when the name holds no part number.
func chipName(s string) string {
	if _, rest, ok := strings.Cut(s, "Family ["); ok {
		if fam, _, ok := strings.Cut(rest, "]"); ok {
			return fam
		}
	}
	for _, w := range strings.Fields(s) {
		w, _, _ = strings.Cut(strings.TrimRight(w, "*,;:"), "/")
		if chipToken.MatchString(w) && digits(w) >= 3 && !unit.MatchString(w) {
			return w
		}
	}
	return ""
}

func digits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// driverChip names a chip when pci.ids is older than it: a Realtek
// Ethernet chip's device ID is its number (0x8127), Realtek's own drivers
// are named for their chip (r8125), as are MediaTek's Wi-Fi ones
// (mt7925e). The kernel's r8169 drives most Realtek chips and names none.
var mtDriver = regexp.MustCompile(`^mt(7[0-9]{3})[a-z]*$`)
var rtlDriver = regexp.MustCompile(`^r(81[0-9]{2})$`)

func driverChip(driver string, vendor, device uint16) string {
	switch vendor {
	case 0x10ec:
		if id := fmt.Sprintf("%04x", device); strings.HasPrefix(id, "81") && digits(id) == 4 {
			return "RTL" + id
		}
		if m := rtlDriver.FindStringSubmatch(driver); m != nil && driver != "r8169" {
			return "RTL" + m[1]
		}
	case 0x14c3:
		if m := mtDriver.FindStringSubmatch(driver); m != nil {
			return "MT" + m[1]
		}
	}
	return ""
}

// windowsNICModel names a Windows network adapter from its driver's
// description ("Realtek Gaming 2.5GbE Family Controller") and its device
// instance ID ("PCI\VEN_10EC&DEV_8125&…"): a PCI card by its vendor and
// chip, as on Linux; anything else by the description alone.
func windowsNICModel(desc, pnp string) string {
	desc = cleanMarks(desc)
	up := strings.ToUpper(pnp)
	if strings.HasPrefix(up, `PCI\`) {
		v, ok1 := pnpHex(up, "VEN_")
		d, ok2 := pnpHex(up, "DEV_")
		if ok1 && ok2 {
			name := desc
			if chipName(desc) == "" {
				if c := driverChip("", v, d); c != "" {
					name = c
				}
			}
			return nicModel(v, "", name)
		}
	}
	return desc
}

// pnpHex reads the four hex digits after a key in a device instance ID.
func pnpHex(id, key string) (uint16, bool) {
	i := strings.Index(id, key)
	if i < 0 || len(id) < i+len(key)+4 {
		return 0, false
	}
	var v uint16
	_, err := fmt.Sscanf(id[i+len(key):i+len(key)+4], "%04X", &v)
	return v, err == nil
}

// cleanMarks drops the registered and trademark marks a Windows driver
// puts in its name: "Intel(R) Wi-Fi 6E AX211" → "Intel Wi-Fi 6E AX211".
func cleanMarks(s string) string {
	for _, m := range []string{"(R)", "(r)", "(TM)", "(tm)", "®", "™"} {
		s = strings.ReplaceAll(s, m, "")
	}
	return strings.Join(strings.Fields(s), " ")
}
