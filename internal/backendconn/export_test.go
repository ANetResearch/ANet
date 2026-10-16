package backendconn

// PeerRefusalsForTest are the listener check's coded refusals for sock, for the external test that
// holds them to the reason table.
func PeerRefusalsForTest(sock string) []error {
	return []error{refusePeerCredUnsupported(sock), refusePeerUIDMismatch(sock, 0, "uid 1000"),
		refusePeerUIDMismatch(sock, 1500, "svc")}
}
