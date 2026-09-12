const questionStart = /^(?:who|what|why|how|when|where|which|can|could|should|is|are|do|does)\b/iu;

export function isQuestionShapedAnswer(value: string): boolean {
  const answer = value.trim();
  return answer.endsWith("?") || questionStart.test(answer);
}
