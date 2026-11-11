import { GRADE_CHIP, clampScore } from "@/lib/format";
import { cn } from "./cn";

// Outline-only chip colors for the grade. The per-clause loss chips (Tag) are filled,
// so the outline keeps the overall grade visually distinct from them.
const OUTLINE_TONES = {
  danger: "border-danger/60 text-danger",
  warn: "border-warn/60 text-warn",
  neutral: "border-ink-3 text-ink-2",
} as const;

/**
 * Overall risk score card for the result screen.
 * - enabled=false (flag off): renders nothing, even when score/grade have values.
 * - score and grade are hidden independently; when both are null the whole card disappears.
 * - Uses `!== null` checks so that a score of 0 is still shown.
 */
export function RiskScore({
  enabled,
  score,
  grade,
}: {
  enabled: boolean;
  score: number | null;
  grade: string | null;
}) {
  // The flag wins over the data: hide everything when it is off, even if values exist.
  if (!enabled) return null;
  // clampScore keeps 0 as a real score and only turns null/NaN into null.
  const shown = clampScore(score);
  const chip = grade !== null ? GRADE_CHIP[grade] : undefined;
  // Nothing to show: return null instead of an empty card so the layout has no gap.
  if (shown === null && !chip) return null;

  return (
    <section
      aria-label="종합 위험 점수"
      data-testid="risk-score"
      className="mt-4 flex flex-col gap-2 rounded-[18px] bg-surface px-4.5 py-5"
    >
      <div className="flex items-center justify-between gap-3">
        <span className="whitespace-nowrap text-[12.5px] font-semibold text-ink-2">종합 위험 점수</span>
        {chip && (
          <span
            data-testid="risk-grade"
            className={cn(
              "whitespace-nowrap rounded-full border px-2.5 py-0.5 text-[10.5px] font-bold",
              OUTLINE_TONES[chip.tone],
            )}
          >
            {chip.label}
          </span>
        )}
      </div>
      {/* Compare with !== null, not truthiness, so a score of 0 is still displayed. */}
      {shown !== null && (
        <span
          data-testid="risk-score-value"
          aria-label={`위험 점수 100점 만점에 ${shown}점`}
          className="whitespace-nowrap text-[26px] font-extrabold tracking-tight text-ink"
        >
          {shown} <span className="text-[15px] font-bold text-ink-3">/ 100</span>
        </span>
      )}
      <span className="text-[11.5px] leading-relaxed text-ink-3">대략적인 추정이며 법률 자문이 아니에요.</span>
    </section>
  );
}

/** "For reference only" badge for clauses whose type has too few samples behind the loss estimate. */
export function LowSampleBadge() {
  return (
    <span
      data-testid="low-sample-badge"
      title="비슷한 분쟁 사례가 적어 참고용으로만 봐 주세요"
      className="whitespace-nowrap rounded-full border border-dashed border-ink-3 px-2 py-0.5 text-[10px] font-bold text-ink-3"
    >
      참고용
    </span>
  );
}
