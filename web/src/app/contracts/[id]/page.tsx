import Link from "next/link";
import { getContract, requireUser } from "@/lib/api/server";
import { SEVERITY_CHIP, clauseLabel, formatDate, formatWon, isLowSample, stripPdf } from "@/lib/format";
import { isRiskScoreEnabled } from "@/lib/features";
import { ConfirmDelete } from "@/components/ui/confirm-delete";
import { Tag } from "@/components/ui/tag";
import { ListRow } from "@/components/ui/list-row";
import { LowSampleBadge, RiskScore } from "@/components/ui/risk-score";
import { AnalysisProgress } from "./analysis-progress";

const DOC_ICON = (
  <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
    <path d="M7 3h7l4 4v14H7z" />
    <path d="M14 3v4h4" />
    <path d="M9.5 13h5M9.5 17h5" />
  </svg>
);

function Shell({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col items-center bg-bg">
      <div className="flex w-full flex-col px-5 md:w-155 md:px-0">
        <div className="flex items-center gap-2 pb-1.5 pt-4.5 md:pt-10">
          <Link href="/home" aria-label="홈으로" className="flex h-9 w-9 items-center justify-center text-ink">
            <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M15 5l-7 7 7 7" />
            </svg>
          </Link>
          <span className="text-[14.5px] font-bold text-ink">{title}</span>
        </div>
        {children}
      </div>
    </div>
  );
}

export default async function ContractResultPage({ params }: { params: Promise<{ id: string }> }) {
  await requireUser();
  const { id } = await params;
  const contract = await getContract(id);
  const clauses = contract.riskItems;

  if (contract.status !== "completed") {
    return (
      <Shell title={contract.status === "pending" ? "분석 중" : "분석 결과"}>
        <div className="flex flex-col gap-1 pt-3">
          <span className="break-all text-[13.5px] font-bold text-ink">{stripPdf(contract.fileName)}</span>
        </div>
        {contract.status === "pending" ? (
          <AnalysisProgress id={contract.id} initial={contract.progress} />
        ) : (
          <div className="flex flex-col items-center gap-4 pt-20 text-center">
            <span className="text-[17px] font-extrabold tracking-tight text-ink">분석하지 못했어요</span>
            <p role="alert" className="max-w-80 text-[13px] leading-relaxed text-danger">
              {contract.error ?? "알 수 없는 문제가 생겼어요"}
            </p>
            <Link href="/analyze" className="mt-2 flex w-full items-center justify-center rounded-2xl bg-accent py-4 text-[15px] font-bold text-white">
              다시 올리기
            </Link>
            <div className="w-full pb-9">
              <ConfirmDelete contractId={contract.id}>기록 지우기</ConfirmDelete>
            </div>
          </div>
        )}
      </Shell>
    );
  }

  const count = clauses.length;
  const warnings = contract.warnings;
  const range = contract.lossRange;
  const sorted = [...clauses].sort((a, b) => b.lossAmount - a.lossAmount);
  // SHOW_RISK_SCORE feature flag, read on every request. Off by default: the score card and the
  // "for reference only" badges stay hidden even when the server returns riskScore/grade values.
  const showScore = isRiskScoreEnabled();

  return (
    <div className="flex min-h-screen flex-col items-center bg-bg">
      <div className="flex w-full flex-col px-5 md:w-155 md:px-0">
        <div className="flex items-center gap-2 pb-1.5 pt-4.5 md:pt-10">
          <Link href="/home" aria-label="홈으로" className="flex h-9 w-9 items-center justify-center text-ink">
            <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M15 5l-7 7 7 7" />
            </svg>
          </Link>
          <span className="text-[14.5px] font-bold text-ink">분석 결과</span>
        </div>

        <div className="flex flex-col gap-1 pt-3">
          <span className="break-all text-[13.5px] font-bold text-ink">{stripPdf(contract.fileName)}</span>
          <span className="text-[11.5px] text-ink-3">{formatDate(new Date(contract.createdAt))} 분석</span>
        </div>

        {/* Overall risk score card, placed above the loss card. It renders nothing when the flag is off
            or when riskScore and grade are both null, so no empty space is left behind. */}
        <RiskScore enabled={showScore} score={contract.riskScore} grade={contract.grade} />

        <div className="pt-4">
          {count === 0 ? (
            <div className="flex flex-col gap-1.5 rounded-[18px] border border-mint/22 bg-mint/8 px-4.5 py-5">
              <span className="text-[16px] font-extrabold text-mint">발견된 위험 조항이 없어요</span>
              <span className="text-[12px] leading-relaxed text-ink-2">
                읽은 조항 안에서는 임차인에게 불리한 내용을 찾지 못했어요. 다만 AI가 놓쳤을 수 있으니 중요한 계약은 전문가에게 확인해 보세요.
              </span>
            </div>
          ) : (
            <div className="flex flex-col gap-4 rounded-[18px] bg-surface px-4.5 py-5">
              <div className="flex items-baseline gap-2">
                <span className="text-[12.5px] font-semibold text-ink-2">발견된 위험 조항</span>
                <span className="text-[26px] font-extrabold tracking-tight text-danger">{count}건</span>
              </div>
              {range && contract.lossAmount > 0 && (
                <div className="flex flex-col gap-1">
                  <span className="text-[12.5px] font-semibold text-ink-2">분쟁이 생기면 예상되는 손실</span>
                  <span className="text-[19px] font-extrabold tracking-tight text-ink">
                    {formatWon(range[0]).replace(/^약 /, "")} ~ {formatWon(range[1])}
                  </span>
                  <span className="text-[11.5px] leading-relaxed text-ink-3">
                    과거 분쟁 조정 사례를 바탕으로 한 참고용 추정이에요. 법률 자문이 아니에요.
                  </span>
                </div>
              )}
            </div>
          )}
        </div>

        {warnings.length > 0 && (
          <ul className="mt-3 flex flex-col gap-1.5 rounded-2xl bg-warn/14 px-4 py-3.5">
            {warnings.map((w: string) => (
              <li key={w} className="text-[12px] leading-relaxed text-warn">
                {w}
              </li>
            ))}
          </ul>
        )}

        {count > 0 && (
          <>
            <div className="pt-5.5">
              <span className="text-[13px] font-bold text-ink">위험 조항</span>
            </div>
            <div className="flex flex-col gap-2.5 pt-3">
              {sorted.map((c) => (
                <ListRow
                  key={c.id}
                  href={`/contracts/${contract.id}/clauses/${c.id}`}
                  icon={DOC_ICON}
                  title={clauseLabel(c.clauseType)}
                  sub={c.original}
                  trailing={
                    <div className="flex flex-col items-end gap-1">
                      <span className="whitespace-nowrap text-xs font-bold text-ink">
                        {c.lossAmount > 0 ? formatWon(c.lossAmount) : "금액 미산출"}
                      </span>
                      {/* Keep the chips on one line: next to the badge on a ~400px screen, the severity chip
                          would otherwise wrap one character per line. */}
                      <div className="flex shrink-0 items-center gap-1 whitespace-nowrap">
                        {/* "For reference only" badge for clause types with too few dispute samples
                            (SAMPLE_COUNT below MIN_SAMPLES in format.ts). Shown only while the flag is on. */}
                        {showScore && isLowSample(c.clauseType) && <LowSampleBadge />}
                        <Tag tone={SEVERITY_CHIP[c.severity].tone}>{SEVERITY_CHIP[c.severity].label}</Tag>
                      </div>
                    </div>
                  }
                />
              ))}
            </div>
          </>
        )}

        <div className="pb-3 pt-6">
          <Link
            href="/analyze"
            className="flex w-full items-center justify-center rounded-2xl bg-accent py-4 text-[15px] font-bold text-white"
          >
            다른 계약서 분석하기
          </Link>
        </div>
        <div className="pb-9">
          <ConfirmDelete contractId={contract.id}>이 계약서 지우기</ConfirmDelete>
        </div>
      </div>
    </div>
  );
}
