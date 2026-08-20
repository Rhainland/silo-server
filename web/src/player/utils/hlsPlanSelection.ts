import type { PlanV3 } from "../protocol-v3";

export function shouldPreferNativeHLSForPlan(plan: PlanV3, nativeHLSSupported: boolean): boolean {
  if (!nativeHLSSupported) return false;
  const profile = (plan.source.video_profile ?? "").toLowerCase().replace(/[\s_-]+/g, "");
  return (
    plan.source.video_codec?.toLowerCase() === "h264" &&
    profile === "high10" &&
    plan.source.bit_depth === 10
  );
}
