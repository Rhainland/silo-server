import { useCallback } from "react";
import type { ItemDetail } from "@/api/types";
import { useToggleFavorite } from "@/hooks/queries/favorites";
import { useWatchedStateMutation } from "@/hooks/queries/items";
import { useDeleteRating, useSetRating } from "@/hooks/queries/ratings";
import { useToggleWatchlist } from "@/hooks/queries/watchlist";
import { getWatchedActionLabel } from "../watchedState";
import ActionBar, { type ActionBarProps } from "./ActionBar";

type UserActionProps =
  | "watchedLabel"
  | "onToggleWatched"
  | "isUpdatingWatched"
  | "onToggleFavorite"
  | "isFavorite"
  | "onToggleWatchlist"
  | "inWatchlist"
  | "rating"
  | "onRatingChange";

interface MediaUserActionBarProps extends Omit<ActionBarProps, UserActionProps> {
  item: ItemDetail;
}

/**
 * Owns the frequently changing mutation state for the detail-page user actions.
 * Keeping these hooks below the page content boundary prevents a mutation's
 * pending/settled notifications from re-rendering the hero and below-fold page.
 */
export default function MediaUserActionBar({ item, ...props }: MediaUserActionBarProps) {
  const isFavorite = item.user_state?.is_favorite ?? false;
  const inWatchlist = item.user_state?.in_watchlist ?? false;
  const watchedMutation = useWatchedStateMutation(item);
  const toggleFavoriteMutation = useToggleFavorite(item.content_id);
  const toggleWatchlistMutation = useToggleWatchlist(item.content_id);
  const { mutate: setRating } = useSetRating(item.content_id);
  const { mutate: deleteRating } = useDeleteRating(item.content_id);

  const handleToggleWatched = useCallback(
    () => watchedMutation.mutate(!(item.user_data?.played ?? false)),
    [item.user_data?.played, watchedMutation],
  );
  const handleToggleFavorite = useCallback(
    () => toggleFavoriteMutation.mutate(isFavorite),
    [isFavorite, toggleFavoriteMutation],
  );
  const handleToggleWatchlist = useCallback(
    () => toggleWatchlistMutation.mutate(inWatchlist),
    [inWatchlist, toggleWatchlistMutation],
  );
  const handleRatingChange = useCallback(
    (rating: number | null) => {
      if (rating === null) {
        deleteRating();
      } else {
        setRating(rating);
      }
    },
    [deleteRating, setRating],
  );

  return (
    <ActionBar
      {...props}
      watchedLabel={getWatchedActionLabel(item)}
      onToggleWatched={handleToggleWatched}
      isUpdatingWatched={watchedMutation.isPending}
      onToggleFavorite={handleToggleFavorite}
      isFavorite={isFavorite}
      onToggleWatchlist={handleToggleWatchlist}
      inWatchlist={inWatchlist}
      rating={item.user_rating ?? null}
      onRatingChange={handleRatingChange}
    />
  );
}
