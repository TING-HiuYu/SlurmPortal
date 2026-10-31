export default function BrandIdentity({ compact = false, name = "Slurm Portal", light = false }) {
  return (
    <span style={{ display: "flex", alignItems: "center", gap: 12, minWidth: 0, color: light ? "#eef2ff" : "var(--console-text)" }}>
      <img src="/branding/logo.svg" alt="" width={34} height={34} style={{ flex: "0 0 34px" }} />
      {!compact && <strong style={{ fontSize: 17, fontWeight: 650, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{name}</strong>}
    </span>
  );
}
