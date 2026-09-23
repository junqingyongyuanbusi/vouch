package sandbox

import "testing"

// Both launchers (unshare on Linux, sandbox-exec on macOS) report their own
// failures as `<name>: <detail>` on the first stderr line. Matching that
// prefix — instead of enumerating message variants — stays robust across
// util-linux versions: runners that restrict unprivileged user namespaces
// changed the wording from "unshare failed" to "write failed
// /proc/self/uid_map".
func TestLooksLikeLauncherFailure(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "unshare uid_map write failure (Ubuntu 24.04 runners)",
			stderr: "unshare: write failed /proc/self/uid_map: Operation not permitted\n",
			want:   true,
		},
		{
			name:   "classic unshare failure wording",
			stderr: "unshare: unshare failed: Operation not permitted\n",
			want:   true,
		},
		{
			name:   "macOS sandbox-exec rejection",
			stderr: "sandbox-exec: sandbox_init failed: Operation not permitted\n",
			want:   true,
		},
		{
			name:   "bare sandbox_init marker (no executable prefix)",
			stderr: "sandbox_init: profile parse error\n",
			want:   true,
		},
		{
			name:   "command-level EPERM is the sandbox working, not a launcher failure",
			stderr: "connect: Operation not permitted\n",
			want:   false,
		},
		{
			name:   "launcher-shaped text later in the stream is not a launcher failure",
			stderr: "warning: about to run\nconnect: Operation not permitted\nunshare: not ours\n",
			want:   false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeLauncherFailure(tc.stderr); got != tc.want {
				t.Fatalf("looksLikeLauncherFailure(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
