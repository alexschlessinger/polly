package sandbox

func finiteNamespaceCancellation(sb Sandbox) bool {
	_, ok := sb.(*linuxSandbox)
	return ok
}
