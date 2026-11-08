package hostmon

import "testing"

func TestNICModel(t *testing.T) {
	cases := []struct {
		vendor           uint16
		vendorName, name string
		want             string
	}{
		// precision's two ports, as pci.ids names them
		{0x8086, "Intel Corporation", "Ethernet Connection (17) I219-LM", "Intel I219-LM"},
		{0x1d6a, "Aquantia Corp.", "AQtion AQC113 NBase-T/IEEE 802.3an Ethernet Controller [Antigua 10G]", "Aquantia AQC113"},
		// a Mac mini's, as their drivers name them
		{0x1d6a, "", "AQC113", "Aquantia AQC113"},
		{0x14e4, "", "BCM4378", "Broadcom BCM4378"},
		{0x10ec, "Realtek Semiconductor Co., Ltd.", "RTL8111/8168/8211/8411 PCI Express Gigabit Ethernet Controller", "Realtek RTL8111"},
		{0x10ec, "Realtek Semiconductor Co., Ltd.", "RTL8125 2.5GbE Controller", "Realtek RTL8125"},
		{0x8086, "Intel Corporation", "Wi-Fi 6E(802.11ax) AX210/AX1675* 2x2 [Typhoon Peak]", "Intel AX210"},
		{0x8086, "Intel Corporation", "Ethernet Controller X710 for 10GbE SFP+", "Intel X710"},
		{0x8086, "Intel Corporation", "82574L Gigabit Network Connection", "Intel 82574L"},
		{0x15b3, "Mellanox Technologies", "MT2910 Family [ConnectX-7]", "Mellanox ConnectX-7"},
		{0x14c3, "MEDIATEK Corp.", "MT7922 802.11ax PCI Express Wireless Network Adapter", "MediaTek MT7922"},
		{0x11ab, "Marvell Technology Group Ltd.", "88E8056 PCI-E Gigabit Ethernet Controller", "Marvell 88E8056"},
		// no part number: the name whole; a maker the table lacks, shortened
		{0x1af4, "Red Hat, Inc.", "Virtio network device", "Red Hat Virtio network device"},
		{0x1234, "Example Networks Inc.", "", "Example Networks"},
		{0x1234, "", "", ""},
	}
	for _, c := range cases {
		if got := nicModel(c.vendor, c.vendorName, c.name); got != c.want {
			t.Errorf("nicModel(%#x, %q, %q) = %q, want %q", c.vendor, c.vendorName, c.name, got, c.want)
		}
	}
}

func TestUSBModel(t *testing.T) {
	for _, c := range [][3]string{
		{"Realtek", "USB 10/100/1000 LAN", "Realtek USB 10/100/1000 LAN"},
		{"ASIX Electronics Corp.", "AX88179A", "ASIX AX88179A"},
		{"Realtek", "Realtek USB GbE Family Controller", "Realtek USB GbE Family Controller"},
		{"", "AX88179", "AX88179"},
	} {
		if got := usbModel(c[0], c[1]); got != c[2] {
			t.Errorf("usbModel(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestDriverChip(t *testing.T) {
	// spark's two ports, newer than its pci.ids
	for _, c := range []struct {
		driver         string
		vendor, device uint16
		want           string
	}{
		{"r8127", 0x10ec, 0x8127, "RTL8127"},
		{"mt7925e", 0x14c3, 0x7925, "MT7925"},
		{"r8169", 0x10ec, 0x8126, "RTL8126"},
		{"mt7921e", 0x14c3, 0x7961, "MT7921"},
		{"r8169", 0x10ec, 0x2600, ""},
		{"r8125", 0x10ec, 0x3000, "RTL8125"},
		{"e1000e", 0x8086, 0x1a1c, ""},
	} {
		if got := driverChip(c.driver, c.vendor, c.device); got != c.want {
			t.Errorf("driverChip(%q, %#x, %#x) = %q, want %q", c.driver, c.vendor, c.device, got, c.want)
		}
	}
}
