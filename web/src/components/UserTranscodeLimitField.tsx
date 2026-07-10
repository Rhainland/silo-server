import { Ban, Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

interface UserTranscodeLimitFieldProps {
  id: string;
  maxTranscodes: number;
  onMaxTranscodesChange: (value: number) => void;
  transcodeAllowed: boolean;
  onTranscodeAllowedChange: (allowed: boolean) => void;
  audioTranscodeAllowed: boolean;
  onAudioTranscodeAllowedChange: (allowed: boolean) => void;
}

export function UserTranscodeLimitField({
  id,
  maxTranscodes,
  onMaxTranscodesChange,
  transcodeAllowed,
  onTranscodeAllowedChange,
  audioTranscodeAllowed,
  onAudioTranscodeAllowedChange,
}: UserTranscodeLimitFieldProps) {
  const audioTranscodeId = `${id}-audio`;

  return (
    <div className="space-y-1">
      <Label htmlFor={id}>Max Transcodes</Label>
      <div className="flex">
        <Input
          id={id}
          type="number"
          min={0}
          value={maxTranscodes}
          disabled={!transcodeAllowed}
          onChange={(event) => onMaxTranscodesChange(Number(event.target.value))}
          className="rounded-r-none"
        />
        <Button
          type="button"
          variant={transcodeAllowed ? "outline" : "secondary"}
          onClick={() => onTranscodeAllowedChange(!transcodeAllowed)}
          aria-label={transcodeAllowed ? "Disable video transcoding" : "Enable video transcoding"}
          aria-pressed={!transcodeAllowed}
          className="rounded-l-none border-l-0"
        >
          {transcodeAllowed ? <Ban /> : <Check />}
          {transcodeAllowed ? "Disable" : "Enable"}
        </Button>
      </div>
      <div className="flex min-h-6 items-center justify-between gap-3">
        <p
          className="text-muted-foreground min-w-0 truncate text-xs"
          title={transcodeAllowed ? undefined : "Video transcoding disabled"}
        >
          {transcodeAllowed ? "0 = unlimited" : "Video transcoding disabled"}
        </p>
        {!transcodeAllowed && (
          <div className="flex shrink-0 items-center gap-2">
            <Label
              htmlFor={audioTranscodeId}
              className="text-muted-foreground text-xs"
              title="Allow audio conversion without video encoding"
            >
              Audio transcodes
            </Label>
            <Switch
              id={audioTranscodeId}
              checked={audioTranscodeAllowed}
              onCheckedChange={onAudioTranscodeAllowedChange}
            />
          </div>
        )}
      </div>
    </div>
  );
}
