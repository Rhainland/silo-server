import { useQuery } from "@tanstack/react-query";
import { captureProfileRequestContext, StaleApiRequestContextError } from "@/api/client";
import {
  adminDownloadPreparationsKey,
  listAdminDownloadPreparations,
} from "@/api/v2/adminDownloadPreparations";

// Realtime events keep the list current; the stale time only bounds how long
// a missed event can hide a change.
const ADMIN_DOWNLOAD_PREPARATIONS_STALE_TIME = 30_000;
// Progress events patch rows in place, and not every change to a job's
// requesters (a profile or device purge, a subscription cleanup) announces
// itself. While work is in flight, re-read the list on this cadence so those
// changes still appear.
export const ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH = 60_000;

/** The offline-download preparation queue, kept live by the admin channel. */
export function useAdminDownloadPreparations() {
  const context = captureProfileRequestContext();
  return useQuery({
    queryKey: adminDownloadPreparationsKey(context),
    queryFn: () => {
      if (!context) throw new StaleApiRequestContextError();
      return listAdminDownloadPreparations(context);
    },
    enabled: context !== null,
    staleTime: ADMIN_DOWNLOAD_PREPARATIONS_STALE_TIME,
    refetchInterval: (query) => {
      const counts = query.state.data?.counts;
      const active = counts ? counts.running + counts.queued + counts.retrying : 0;
      return active > 0 ? ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH : false;
    },
  });
}
