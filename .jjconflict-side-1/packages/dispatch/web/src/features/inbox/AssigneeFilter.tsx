import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { usersQuery } from "../../api/queries";
import { MultiSelect } from "../../components/MultiSelect";
import { EVERYONE_FILTER_VALUE, isOwnList, UNASSIGNED_FILTER_VALUE } from "./sections";

function triggerText(selected: readonly string[], viewer: string): string {
  if (selected.length === 0) return "Assignee · everyone";
  if (isOwnList(selected, viewer)) return "Assignee · you";
  if (selected.length > 1) return `Assignee · ${selected.length}`;
  const only = selected[0];
  return `Assignee · ${only === UNASSIGNED_FILTER_VALUE ? "Unassigned" : only}`;
}

/**
 * Who the Inbox is listed for: the `?assignee=` filter, above the list. The trigger says the
 * selection in the reader's own words (`Assignee · you`, `· everyone`, `· Unassigned`,
 * one other person's email, or how many), and the popover picks any number of people, with
 * Unassigned - every document ask and every issue nobody holds - first.
 */
export function AssigneeFilter({
  onChange,
  onOpenChange,
  open,
  selected,
  viewer,
}: {
  onChange: (selected: readonly string[]) => void;
  onOpenChange: (open: boolean) => void;
  open: boolean;
  /** Who the list is filtered to: the reader's own email alone is the default, an empty array the cleared filter. */
  selected: readonly string[];
  /** The signed-in person's lowercase email. */
  viewer: string;
}): ReactNode {
  // Every Inbox reader renders this control and few open it, so the people are fetched on the
  // first open and kept from then on: a reader who never filters costs no `/api/v1/users`.
  const [wanted, setWanted] = useState(false);
  const people = useQuery({ ...usersQuery(), enabled: wanted });

  // A login equal to either reserved word would be a row that means the filter rather than the
  // person, so neither is offered among the people.
  const reserved: readonly string[] = [UNASSIGNED_FILTER_VALUE, EVERYONE_FILTER_VALUE];
  const listed = [
    UNASSIGNED_FILTER_VALUE,
    ...(people.data ?? [])
      .map((person) => person.login)
      .filter((login) => !reserved.includes(login)),
  ];
  // A selected value the list does not hold - a person who has not signed in, or anyone at all
  // while the request is out or refused - is still a row, so a filter read from the address
  // always shows what it is filtering by and the reader can switch it off.
  const options = [...listed, ...selected.filter((value) => !listed.includes(value))];

  return (
    <MultiSelect
      emptyMessage={people.isPending ? "Loading people…" : "No matching person."}
      error={people.isError ? "Could not load the people list." : null}
      label="Assignee"
      onChange={onChange}
      onOpenChange={(next) => {
        if (next) setWanted(true);
        onOpenChange(next);
      }}
      open={open}
      optionDetail={(value) => (value === viewer ? "you" : undefined)}
      optionLabel={(value) => (value === UNASSIGNED_FILTER_VALUE ? "Unassigned" : value)}
      options={options}
      searchLabel="Search people"
      selected={selected}
      triggerAriaLabel="Filter by assignee"
    >
      {triggerText(selected, viewer)}
    </MultiSelect>
  );
}
