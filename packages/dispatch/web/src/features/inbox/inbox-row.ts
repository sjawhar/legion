/** The attribute every Inbox ask row carries, valued with its ask id: the list's keyboard, viewport
 *  and jump target. Group headers and band labels never carry it. */
export const ROW_ATTRIBUTE = "data-inbox-row";

/** Every Inbox ask row. */
export const ROW_SELECTOR = `[${ROW_ATTRIBUTE}]`;
