//go:build race

package visualstore_test

// raceBuild trims the exactness replay under the race detector, where the
// full per-prefix page and seek sweep exceeds the package timeout. The
// untagged run keeps the full sweep.
const raceBuild = true
