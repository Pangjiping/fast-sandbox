package main

import (
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func validateMemoryReclaim(p *apiv1alpha2.MemoryReclaimSpec) error {
	if p == nil {
		return nil
	}
	if p.DamonReclaim && !p.FreePageReporting {
		return fmt.Errorf("damonReclaim requires freePageReporting")
	}
	if p.MinAgeSeconds < 0 || p.MinAgeSeconds > 3600 || p.QuotaMiB < 0 || p.QuotaMiB > 1024 {
		return fmt.Errorf("memoryReclaim age/quota is out of range")
	}
	return nil
}

// Requested features must be ready before the golden snapshot is captured.
// DAMON acts inside the guest, never on the host's physical address space.
func renderMemoryReclaim(p *apiv1alpha2.MemoryReclaimSpec) string {
	if p == nil || !p.FreePageReporting {
		return ""
	}
	script := `
if [ ! -d /sys/module/virtio_balloon ] || [ ! -d /sys/module/page_reporting ]; then
  echo "SANDBOX_STARTUP_FAILED free_page_reporting_unsupported"
  exit 1
fi
echo SANDBOX_FREE_PAGE_REPORTING_READY
`
	if !p.DamonReclaim {
		return script
	}
	age, quota := p.MinAgeSeconds, p.QuotaMiB
	if age == 0 {
		age = 60
	}
	if quota == 0 {
		quota = 64
	}
	return script + fmt.Sprintf(`
damon=/sys/module/damon_reclaim/parameters
for parameter in enabled skip_anon min_age quota_ms quota_sz quota_reset_interval_ms wmarks_high wmarks_mid wmarks_low; do
  if [ ! -w "$damon/$parameter" ]; then
    echo "SANDBOX_STARTUP_FAILED damon_reclaim_unsupported_$parameter"
    exit 1
  fi
done
echo N > "$damon/enabled"
echo Y > "$damon/skip_anon"
echo %d > "$damon/min_age"
echo 10 > "$damon/quota_ms"
echo %d > "$damon/quota_sz"
echo 1000 > "$damon/quota_reset_interval_ms"
echo 1000 > "$damon/wmarks_high"
echo 1000 > "$damon/wmarks_mid"
echo 0 > "$damon/wmarks_low"
echo Y > "$damon/enabled"
echo SANDBOX_DAMON_RECLAIM_READY
`, int64(age)*1000000, int64(quota)*1024*1024)
}
