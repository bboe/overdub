package device

func Reboot() error {
	return setProp("sys.powerctl", "reboot")
}
