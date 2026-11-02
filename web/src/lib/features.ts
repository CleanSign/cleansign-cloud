/**
 * Feature flags read from server-side environment variables.
 * Call these only from server components: they read process.env on every request,
 * so changing the variable and restarting the server is enough (no rebuild needed).
 */

/** Show the overall risk score and grade on the result screen. Off unless SHOW_RISK_SCORE is exactly "true". */
export function isRiskScoreEnabled(): boolean {
  return process.env.SHOW_RISK_SCORE === "true";
}
