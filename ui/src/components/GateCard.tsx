import { useState } from "react";
import { useTranslation } from "react-i18next";

/** Human-approval gate rendered inline in a conversation/execution flow. */
export function GateCard({
  question,
  onRespond,
}: {
  question: string;
  onRespond: (approve: boolean) => void;
}) {
  const { t } = useTranslation();
  const [resolved, setResolved] = useState<"approved" | "denied" | null>(null);

  const respond = (approve: boolean) => {
    setResolved(approve ? "approved" : "denied");
    onRespond(approve);
  };

  return (
    <div className="gate-card">
      <div className="gate-card__q">{question}</div>
      {resolved ? (
        <div className="gate-card__resolved">{t(`gate.${resolved}`)}</div>
      ) : (
        <div className="gate-card__actions">
          <button className="btn btn--sm" onClick={() => respond(true)}>
            {t("gate.approve")}
          </button>
          <button className="btn btn--ghost btn--sm" onClick={() => respond(false)}>
            {t("gate.deny")}
          </button>
        </div>
      )}
    </div>
  );
}
