import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { createShuffle, getShuffle, type ShuffleScopeRequest } from "@/api/v2/shuffles";
import { V2ProblemError } from "@/api/v2/request";
import { useWatchPlaybackController } from "@/playback/watchPlaybackContext";
import { shuffleKeys } from "./keys";

/** The running shuffle a playback belongs to: what plays now and next. */
export function useShuffle(shuffleId: string | undefined) {
  return useQuery({
    queryKey: shuffleKeys.detail(shuffleId ?? ""),
    queryFn: ({ signal }) => getShuffle(shuffleId!, signal),
    enabled: !!shuffleId,
    staleTime: Number.POSITIVE_INFINITY,
  });
}

function startShuffleErrorMessage(error: unknown): string {
  if (error instanceof V2ProblemError && error.status === 409) {
    return "Nothing here can be played.";
  }
  return "Couldn't start shuffle.";
}

/**
 * Starts a shuffle over a scope and plays its first pick from the beginning.
 * Leaving the player returns to the page the shuffle started from.
 */
export function useStartShuffle() {
  const queryClient = useQueryClient();
  const controller = useWatchPlaybackController();
  const mutation = useMutation({
    mutationFn: (scope: ShuffleScopeRequest) => createShuffle(scope),
    onSuccess: (shuffle) => {
      queryClient.setQueryData(shuffleKeys.detail(shuffle.id), shuffle);
      controller.startPlayback(
        {
          contentId: shuffle.current.content_id,
          shuffleId: shuffle.id,
          // Shuffle picks always play from the beginning, not saved progress.
          restart: true,
          returnHref: `${window.location.pathname}${window.location.search}`,
        },
        "viewer",
      );
    },
    onError: (error) => {
      toast.error(startShuffleErrorMessage(error));
    },
  });
  return {
    startShuffle: (scope: ShuffleScopeRequest) => mutation.mutate(scope),
    isStarting: mutation.isPending,
  };
}
