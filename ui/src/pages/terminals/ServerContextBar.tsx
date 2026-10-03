import { NavLink } from "react-router-dom";
import { serverPath, type ServerSection } from "@/lib/terminalsHost";
import { cn } from "@/lib/utils";

const SECTIONS: Array<{ section: ServerSection; label: string }> = [
  { section: "tasks", label: "Tasks" },
  { section: "images", label: "Images" },
  { section: "workflows", label: "Workflow images" },
  { section: "stores", label: "Stores" },
  { section: "settings", label: "Settings" },
];

/** The island's header for server-scoped views. Same strip as the agent tabs —
 *  34px, a 2px --primary underline on the active section — so the two ways of
 *  entering the island read as one control surface. */
export function ServerContextBar({ hostId, label }: { hostId: string; label: string }) {
  return (
    <nav
      aria-label="Server workspace"
      className="flex shrink-0 items-center gap-0.5 border-b px-3"
    >
      <span className="mr-2 flex min-w-0 items-baseline gap-1.5 text-[12.5px]">
        <span className="text-muted-foreground">Server:</span>
        <span className="truncate font-medium" title={label}>{label}</span>
      </span>
      {SECTIONS.map(({ section, label: sectionLabel }) => (
        <NavLink
          key={section}
          to={serverPath(hostId, section)}
          className={({ isActive }) => cn(
            "flex h-[34px] items-center border-b-2 border-transparent px-2.5 text-[13px] text-muted-foreground hover:text-foreground",
            isActive && "border-primary font-medium text-foreground",
          )}
        >
          {sectionLabel}
        </NavLink>
      ))}
    </nav>
  );
}
