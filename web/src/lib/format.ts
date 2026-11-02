/** Format a KRW amount for readability: 3,612,000 → "약 361만 원", 150,000,000 → "약 1.5억 원" */
export function formatWon(n: number): string {
  if (n <= 0) return "0원";
  if (n >= 100_000_000) {
    const eok = Math.round(n / 10_000_000) / 10;
    return `약 ${eok}억 원`;
  }
  if (n >= 10_000) return `약 ${Math.round(n / 10_000).toLocaleString("ko-KR")}만 원`;
  return `약 ${n.toLocaleString("ko-KR")}원`;
}

export function formatDate(d: Date): string {
  const parts = new Intl.DateTimeFormat("ko-KR", {
    timeZone: "Asia/Seoul",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).formatToParts(d);
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? "";
  return `${get("year")}.${get("month")}.${get("day")}`;
}

export const stripPdf = (name: string) => name.replace(/\.pdf$/i, "");

export const CLAUSE_LABEL: Record<string, string> = {
  선순위근저당: "선순위 근저당",
  보증금반환제한: "보증금 반환 제한",
  원상복구과도부담: "원상복구 과도 부담",
  묵시적갱신배제: "갱신 권리 제한",
  기타불이익: "기타 불리한 조항",
};
export const clauseLabel = (t: string | null) => (t && CLAUSE_LABEL[t]) || "위험 조항";

/**
 * Clause chip. severity is not 'how legally bad it is' but how large the expected loss is relative to the deposit (loss scale).
 * So we use "loss large/medium/small" rather than "risk high/low", and never color the small end green (safe).
 */
export const SEVERITY_CHIP: Record<string, { label: string; tone: "danger" | "warn" | "neutral" }> = {
  high: { label: "손실 큼", tone: "danger" },
  medium: { label: "손실 보통", tone: "warn" },
  low: { label: "손실 작음", tone: "neutral" },
};

/**
 * Overall contract grade chip. Worded as "risk level" so it is not confused with the per-clause
 * loss-size chip (SEVERITY_CHIP), and drawn as an outline chip instead of a filled one.
 * As with SEVERITY_CHIP, the low end is never colored green (safe).
 */
export const GRADE_CHIP: Record<string, { label: string; tone: "danger" | "warn" | "neutral" }> = {
  high: { label: "위험도 높음", tone: "danger" },
  medium: { label: "위험도 보통", tone: "warn" },
  low: { label: "위험도 낮음", tone: "neutral" },
};

/** Clamp a risk score into 0-100 for display. Returns null for a missing or non-finite value. 0 is a valid score. */
export function clampScore(score: number | null): number | null {
  if (score === null || !Number.isFinite(score)) return null;
  return Math.min(100, Math.max(0, Math.round(score)));
}

/**
 * Number of labeled dispute cases per clause type behind the loss estimate.
 * Source: data/labeling/severity_dataset_adrhome.csv + severity_dataset_hldcc.csv (rows per clause_type_mapping).
 * Update together with the labeling data.
 */
export const SAMPLE_COUNT: Record<string, number> = {
  선순위근저당: 0,
  보증금반환제한: 0,
  원상복구과도부담: 18,
  묵시적갱신배제: 19,
  기타불이익: 0,
};

/**
 * PLACEHOLDER threshold: fewer samples than this marks a clause as "for reference only".
 * Taken from the per-type minimum in data/labeling/README.md (rule 4). Do not change it without the data team.
 */
export const MIN_SAMPLES = 15;

/** True when the clause type has too few samples. An unknown or unclassified type counts as having none. */
export const isLowSample = (clauseType: string | null) => (clauseType ? (SAMPLE_COUNT[clauseType] ?? 0) : 0) < MIN_SAMPLES;
