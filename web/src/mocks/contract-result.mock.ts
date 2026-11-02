/**
 * Personal mock data for checking the contract analysis result screen (/contracts/[id]).
 *
 * - A per-case reference table for designing the overall risk score (riskScore) and grade display.
 * - All scores, amounts, and clause texts are made-up fake values, unrelated to any real contract or personal data.
 * - Not imported anywhere in the app code. Reference material for checking the screen / Figma designs.
 *
 * Differences from the schema
 * - RiskItem.lowSample: temporary mock-only field. It does not exist in openapi.yaml / schema.d.ts.
 *   The server does not return it yet, so the schema must be updated before the screen can use it.
 * - case / expect: per-case metadata. Kept in a wrapper outside Contract.
 */
import type { Contract, RiskItem } from "@/lib/api/types";

/** Keys of CLAUSE_LABEL in format.ts. Any other string is a type error. */
export type KnownClauseType = "선순위근저당" | "보증금반환제한" | "원상복구과도부담" | "묵시적갱신배제" | "기타불이익";

/** Mock-only RiskItem: narrows clauseType to known values and allows the temporary lowSample (insufficient sample) field. */
export type MockRiskItem = Omit<RiskItem, "clauseType"> & { clauseType: KnownClauseType | null; lowSample?: boolean };
export type MockContract = Omit<Contract, "riskItems"> & { riskItems: MockRiskItem[] };

export type ContractMockCase = {
  case: string;
  /** One-line expected result: how the screen should look for this case. */
  expect: string;
  contract: MockContract;
};

const CREATED_AT = "2026-10-01T09:00:00+09:00";
const DEPOSIT = 100_000_000;

/* ---------- Shared clauses (fake text) ---------- */

const ITEM_DEPOSIT_RETURN: MockRiskItem = {
  id: "mock-clause-01a",
  original: "임대인은 새 임차인이 입주하여 보증금을 지급한 후에만 기존 임차인에게 보증금을 반환한다.",
  clauseType: "보증금반환제한",
  confidence: 0.92,
  severity: "high",
  lossAmount: 30_000_000,
  reason: "보증금 반환 시점을 새 임차인 입주에 묶어 두어, 공실이 길어지면 보증금을 제때 돌려받지 못할 수 있어요.",
  lawReference: "주택임대차보호법 제3조의2, 민법 제536조",
  safeText: "임대인은 계약 종료일에 새 임차인 입주 여부와 관계없이 보증금 전액을 반환한다.",
};

const ITEM_RESTORE: MockRiskItem = {
  id: "mock-clause-01b",
  original: "임차인은 퇴거 시 입주 당시 상태와 관계없이 도배·장판·설비 전부를 새것으로 교체한다.",
  clauseType: "원상복구과도부담",
  confidence: 0.81,
  severity: "medium",
  lossAmount: 4_500_000,
  reason: "통상적인 사용으로 생긴 마모까지 임차인이 부담하도록 해 원상복구 범위를 넘어서요.",
  lawReference: "민법 제615조, 민법 제654조",
  safeText: "임차인은 고의 또는 과실로 인한 훼손에 한하여 원상복구 의무를 진다. 통상의 손모는 제외한다.",
};

const ITEM_RENEWAL: MockRiskItem = {
  id: "mock-clause-01c",
  original: "본 계약은 기간 만료 시 자동으로 종료되며 임차인은 갱신을 요구할 수 없다.",
  clauseType: "묵시적갱신배제",
  confidence: 0.67,
  severity: "low",
  lossAmount: 1_200_000,
  reason: "법이 보장하는 계약갱신요구권과 묵시적 갱신을 미리 포기하게 하는 조항이에요.",
  lawReference: "주택임대차보호법 제6조, 주택임대차보호법 제6조의3",
  safeText: "계약 기간 만료 시 임차인은 주택임대차보호법에 따라 계약 갱신을 요구할 수 있다.",
};

const BASE_01_ITEMS: MockRiskItem[] = [ITEM_DEPOSIT_RETURN, ITEM_RESTORE, ITEM_RENEWAL];

/** Shared body so that cases 01 and 03 are identical except for score and grade. */
const BASE_01: Omit<MockContract, "riskScore" | "grade"> = {
  id: "mock-contract-01",
  status: "completed",
  progress: null,
  error: null,
  fileName: "샘플_임대차계약서_A.pdf",
  createdAt: CREATED_AT,
  lossAmount: 35_700_000,
  lossRange: [21_000_000, 35_700_000],
  depositAmount: DEPOSIT,
  flow: null,
  warnings: [],
  riskItems: BASE_01_ITEMS,
};

/* ---------- Cases ---------- */

export const contractResultMocks: ContractMockCase[] = [
  {
    case: "01-high-score",
    expect: "점수 72와 '높음' 등급이 보이고, 위험 조항 3건이 손실 금액 큰 순(보증금 반환 제한 → 원상복구 → 갱신)으로 나온다.",
    contract: { ...BASE_01, riskScore: 72, grade: "high" },
  },
  {
    case: "02-low-score",
    expect: "점수 18과 '낮음' 등급이 보이고, 위험 조항 1건과 예상 손실 범위가 나온다. 낮음이어도 초록(안전)색으로 칠하지 않는다.",
    contract: {
      id: "mock-contract-02",
      status: "completed",
      progress: null,
      error: null,
      fileName: "샘플_임대차계약서_B.pdf",
      createdAt: CREATED_AT,
      riskScore: 18,
      grade: "low",
      lossAmount: 800_000,
      lossRange: [300_000, 800_000],
      depositAmount: DEPOSIT,
      flow: null,
      warnings: [],
      riskItems: [
        {
          id: "mock-clause-02a",
          original: "임차인은 임대인의 사전 동의 없이 벽에 못을 박거나 선반을 설치할 수 없으며, 위반 시 수리비 전액을 부담한다.",
          clauseType: "기타불이익",
          confidence: 0.58,
          severity: "low",
          lossAmount: 800_000,
          reason: "경미한 설치 행위까지 수리비 전액 부담으로 묶어 두어 임차인에게 다소 불리해요.",
          lawReference: null,
          safeText: "임차인이 설치한 시설로 인한 훼손은 퇴거 시 통상적인 범위에서 원상복구한다.",
        },
      ],
    },
  },
  {
    case: "03-null-score",
    expect: "flag OFF 시뮬레이션. 점수·등급 영역이 아예 보이지 않고(0이나 '-'로도 표시 안 함), 나머지는 01과 똑같이 나온다.",
    contract: { ...BASE_01, riskScore: null, grade: null },
  },
  {
    case: "04-low-sample",
    expect: "점수 55와 '보통' 등급, 경고 문구 1개가 보인다. lowSample 조항에는 '참고 사례가 적음' 같은 표시가 붙어야 한다(스키마에 없는 임시 필드).",
    contract: {
      id: "mock-contract-04",
      status: "completed",
      progress: null,
      error: null,
      fileName: "샘플_임대차계약서_C.pdf",
      createdAt: CREATED_AT,
      riskScore: 55,
      grade: "medium",
      lossAmount: 14_000_000,
      lossRange: [6_000_000, 14_000_000],
      depositAmount: DEPOSIT,
      flow: null,
      warnings: ["일부 조항은 비슷한 분쟁 사례가 적어 예상 손실이 부정확할 수 있어요."],
      riskItems: [
        {
          id: "mock-clause-04a",
          original: "계약 체결 이후 설정되는 담보권에 대해 임대인은 임차인에게 고지할 의무가 없다.",
          clauseType: "선순위근저당",
          confidence: 0.74,
          severity: "medium",
          lossAmount: 11_000_000,
          reason: "보증금보다 앞서는 담보권이 생겨도 알 수 없어 보증금 회수 순위가 밀릴 수 있어요.",
          lawReference: "주택임대차보호법 제3조의2, 주택임대차보호법 제8조",
          safeText: "임대인은 계약 기간 중 담보권을 설정하려면 사전에 임차인에게 서면으로 알리고 동의를 받는다.",
        },
        {
          id: "mock-clause-04b",
          original: "관리비 항목과 금액은 임대인이 필요에 따라 별도 통지 없이 변경할 수 있다.",
          clauseType: "기타불이익",
          confidence: 0.49,
          severity: "low",
          lossAmount: 3_000_000,
          reason: "관리비를 일방적으로 올릴 수 있어 예상하지 못한 비용이 생길 수 있어요.",
          lawReference: null,
          safeText: "관리비 항목과 금액을 변경하려면 임차인에게 사전에 서면으로 알리고 협의한다.",
          lowSample: true,
        },
      ],
    },
  },
  {
    case: "05-zero-risk",
    expect: "점수 0과 '낮음' 등급이 보인다(null처럼 숨기면 안 됨). '발견된 위험 조항이 없어요' 안내가 나오고 손실 금액 영역은 없다.",
    contract: {
      id: "mock-contract-05",
      status: "completed",
      progress: null,
      error: null,
      fileName: "샘플_임대차계약서_D.pdf",
      createdAt: CREATED_AT,
      riskScore: 0,
      grade: "low",
      lossAmount: 0,
      lossRange: null,
      depositAmount: DEPOSIT,
      flow: null,
      warnings: [],
      riskItems: [],
    },
  },
  {
    case: "06-pending",
    expect: "'분석 중' 화면과 진행 단계(classifying 3/8)가 보이고, 점수·등급·조항 목록은 보이지 않는다.",
    contract: {
      id: "mock-contract-06",
      status: "pending",
      progress: { stage: "classifying", done: 3, total: 8 },
      error: null,
      fileName: "샘플_임대차계약서_E.pdf",
      createdAt: CREATED_AT,
      riskScore: null,
      grade: null,
      lossAmount: 0,
      lossRange: null,
      depositAmount: null,
      flow: null,
      warnings: [],
      riskItems: [],
    },
  },
  {
    case: "07-failed",
    expect: "'분석하지 못했어요'와 error 메시지, '다시 올리기' 버튼이 보이고, 점수·등급은 보이지 않는다.",
    contract: {
      id: "mock-contract-07",
      status: "failed",
      progress: null,
      error: "PDF에서 글자를 읽지 못했어요. 스캔 상태를 확인하고 다시 올려 주세요.",
      fileName: "샘플_임대차계약서_F.pdf",
      createdAt: CREATED_AT,
      riskScore: null,
      grade: null,
      lossAmount: 0,
      lossRange: null,
      depositAmount: null,
      flow: null,
      warnings: [],
      riskItems: [],
    },
  },
];

/* ---------- Type check ---------- */

// Verifies that every case, aside from the mock-only lowSample field, fits the real Contract response type as is.
export const contractResultMocksAsApi = contractResultMocks.map((m) => m.contract) satisfies Contract[];
