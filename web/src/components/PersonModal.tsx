// Person details modal.
// Opens from a click on the people list (via the modal service) — for any
// viewer (signed-in user or share visitor). Shows the person's optional
// metadata — aliases, partial birthdate, reference URLs and a markdown
// description — and hides every field that was never filled; a person with
// no metadata shows name, photo count and a "no details recorded" hint.
//
// Writer extras (rendered only for writer/admin of the person's repo):
//  - "Edit details" turns the body into an inline form and saves through
//    POST /api/people/{name}/meta (full-replace semantics);
//  - "Manage photos" opens the photos-manager modal stacked on top of this
//    one (the photos grid itself stays writer-only).
// A share visitor gets none of these — the modal is purely read-only.
//
// The person's avatar comes from the live people list (useApp): when the
// photos manager changes the avatar underneath, the (paused) details modal
// re-renders with the new thumbnail.

import { useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { PersonDetail } from "../types";
import { plural } from "../util";
import { renderMarkdown } from "../markdown";
import {
	BIRTH_FORMATS_HINT,
	birthAgeLine,
	formatBirthText,
	formatBirthTextual,
	parseBirthText,
} from "../birthdate";
import { useToast } from "../toast";
import { useApp } from "../appStore";
import { useModals } from "../modalRegistry";
import { Avatar } from "./Avatar";
import { MetaRows } from "./MetaRows";
import { Modal } from "./Modal";

interface PersonModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
	/** the person whose details are shown */
	name: string;
}

export function PersonModal(props: PersonModalProps) {
	const toast = useToast();
	const app = useApp();
	const modals = useModals();
	const { name: person, open, onCloseRequest } = props;

	const [detail, setDetail] = useState<PersonDetail | null>(null);
	const [loadHint, setLoadHint] = useState("Loading details…");
	const [editing, setEditing] = useState(false);
	const [busyState, setBusyState] = useState(false);

	// Edit-form state (kept flat so inputs stay simple and controlled).
	const [aliases, setAliases] = useState<string[]>([]);
	const [urls, setUrls] = useState<string[]>([]);
	// One dashed-template text field: YYYY-MM-DD / YYYY--DD / YYYY-MM /
	// YYYY / -MM-DD / -MM- (see birthdate.ts).
	const [birthField, setBirthField] = useState("");
	const [description, setDescription] = useState("");

	const personRef = useRef(person);
	const busyRef = useRef(false);
	const formRef = useRef<HTMLFormElement | null>(null);
	const repoIdRef = useRef(app.repoId ?? "");
	repoIdRef.current = app.repoId ?? "";

	personRef.current = person;

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	// Open: load the details.
	useEffect(() => {
		setDetail(null);
		setLoadHint("Loading details…");
		setEditing(false);
		loadDetail(person);
	}, [person]);

	// Editing: focus the form's first input.
	useEffect(() => {
		if (editing) formRef.current?.querySelector<HTMLElement>("input, textarea")?.focus();
	}, [editing]);

	// Drop responses for a person the user already left behind.
	async function loadDetail(name: string) {
		try {
			const j = await api.getPerson(name, repoIdRef.current);
			if (personRef.current !== name) return;
			setDetail(j);
			// Share mode learns the repository's human name from the details
			// document (the app store hooks it; MainApp leaves it unset).
			app.onPersonDetail?.(j);
		} catch (e) {
			if (personRef.current === name) {
				setLoadHint((e as Error).message || "Could not load the details.");
			}
		}
	}

	const close = () => {
		if (busyRef.current) return; // locked while a request is in flight
		onCloseRequest();
	};

	// Escape cancels the edit session first, then closes.
	const onEscape = () => {
		if (editing) cancelEdit();
		else close();
	};

	// ---- view helpers ----
	const meta = detail;
	const birthText = formatBirthTextual(meta?.birthdate);
	// The same date in dashed numeric form (YYYY-MM-DD when complete).
	const birthIso = formatBirthText(meta?.birthdate);
	const birthExtra = birthAgeLine(meta?.birthdate);
	const hasMeta = !!(meta && ((meta.aliases?.length ?? 0) > 0 || birthText ||
		(meta.urls?.length ?? 0) > 0 || (meta.description ?? "") !== ""));

	function startEdit() {
		if (busyRef.current || editing || !detail) return;
		// The lists keep one trailing empty row as the always-ready slot;
		// blank rows (including that slot) are filtered out before saving.
		setAliases([...(detail.aliases ?? []), ""]);
		setUrls([...(detail.urls ?? []), ""]);
		setBirthField(formatBirthText(detail.birthdate));
		setDescription(detail.description ?? "");
		setEditing(true);
	}

	function cancelEdit() {
		if (busyRef.current) return;
		setEditing(false);
	}

	useEffect(() => {
		if (!editing) return;
		const onUnload = (e: BeforeUnloadEvent) => {
			e.preventDefault();
			e.returnValue = ""; // legacy Chrome/Safari; modern engines ignore custom text
		};
		window.addEventListener("beforeunload", onUnload);
		return () => window.removeEventListener("beforeunload", onUnload);
	}, [editing]);

	const saveEdit = async (e: Event) => {
		e.preventDefault();
		const name = personRef.current;
		if (busyRef.current || !name) return;
		// The birthdate text field is validated client-side (format +
		// component ranges); the server re-checks ranges and calendars.
		const birth = parseBirthText(birthField);
		if (!birth.ok) {
			toast.show(birth.error, "err");
			return;
		}
		const parts = birth.parts;
		const birthdate = parts.year == null && parts.month == null && parts.day == null
			? null
			: { year: parts.year ?? null, month: parts.month ?? null, day: parts.day ?? null };
		setBusy(true);
		try {
			const j = await api.setPersonMeta(name, repoIdRef.current, {
				aliases: aliases.map((a) => a.trim()).filter(Boolean),
				birthdate,
				urls: urls.map((u) => u.trim()).filter(Boolean),
				description,
			});
			setDetail(j);
			setEditing(false);
			toast.show("Details saved.", "ok");
			app.refreshAll(); // the people list summary carries aliases
		} catch (err) {
			toast.show((err as Error).message || "Could not save the details.", "err");
		} finally {
			setBusy(false);
		}
	};

	function managePhotos() {
		if (busyRef.current || !personRef.current) return;
		// Stacked on top: this modal stays mounted (paused, state intact).
		modals.open("photos", { person: personRef.current });
	}

	// The list row editors themselves are the shared MetaRows component.
	// The avatar follows the live people list (alias/photo changes re-render
	// this modal with the new thumbnail URL).
	const thumb = app.people.find((p) => p.name === person)?.thumb ?? null;

	// ---- writer-only header actions ----
	const writerActions = app.perms.writer && (
		<>
			<button type="button" class="btn btn-ghost btn-sm" id="personEditBtn" hidden={editing} onClick={startEdit}>
				Edit details
			</button>
			<button type="button" class="btn btn-ghost btn-sm" id="personPhotosBtn" onClick={managePhotos}>
				Manage photos
			</button>
		</>
	);

	return (
		<Modal
			open={open}
			id="personModal"
			cardId="personCard"
			locked={busyState}
			backdropClickDisabled={editing}
			eyebrow="person"
			titleId="personTitle"
			title={detail?.name ?? person}
			headerActions={writerActions ?? undefined}
			onEscape={onEscape}
			onCloseRequest={close}
			body={
				<div class="modal-body person-detail">
					{!detail && <p class="field-hint" id="personHint">{loadHint}</p>}

					{detail && !editing && (
						<div id="personDetail">
							<div class="person-detail-head">
								<Avatar src={thumb ?? ""} name={detail.name} />
								<p class="field-hint">{plural(detail.photos)} enrolled</p>
							</div>

							{!hasMeta && (
								<p class="field-hint person-empty" id="personEmptyHint">
									No details recorded for this person.
									{app.perms.writer ? " Use “Edit details” to add some." : ""}
								</p>
							)}

							{hasMeta && (
								<div class="person-meta-list">
									{detail.aliases && detail.aliases.length > 0 && (
										<section class="person-section">
											<h3 class="person-section-label">Aliases</h3>
											<div class="person-aliases">
												{detail.aliases.map((a) => <span class="person-alias" key={a}>{a}</span>)}
											</div>
										</section>
									)}

									{birthText && (
										<section class="person-section">
											<h3 class="person-section-label">Born</h3>
											<p class="person-birth">
												{birthText}
												{birthIso && <span class="person-birth-iso"> ({birthIso})</span>}
											</p>
											{birthExtra && <p class="person-birth-extra">{birthExtra}</p>}
										</section>
									)}

									{detail.urls && detail.urls.length > 0 && (
										<section class="person-section">
											<h3 class="person-section-label">Links</h3>
											<ul class="person-links">
												{detail.urls.map((u) => (
													<li key={u}>
														<a href={u} target="_blank" rel="noopener noreferrer">{u}</a>
													</li>
												))}
											</ul>
										</section>
									)}

									{(detail.description ?? "") !== "" && (
										<section class="person-section">
											<h3 class="person-section-label">Notes</h3>
											{/* markdown.ts sanitizes before this ever hits the DOM */}
											<div
												class="person-prose"
												dangerouslySetInnerHTML={{ __html: renderMarkdown(detail.description ?? "") }}
											/>
										</section>
									)}
								</div>
							)}
						</div>
					)}

					{detail && editing && (
						<form id="personMetaForm" class="person-edit" ref={formRef} onSubmit={saveEdit}>
							<label class="person-field">
								<span class="person-field-label">Aliases</span>
								<MetaRows entries={aliases} onChange={setAliases} label="Alias" placeholder="Alias" maxLength={120} disabled={busyState} />
							</label>

							<label class="person-field">
								<span class="person-field-label">Birthdate — leave empty if unknown</span>
								<input
									id="personBirthText"
									type="text"
									placeholder="1965-03-02"
									autoComplete="off"
									spellcheck={false}
									disabled={busyState}
									value={birthField}
									onInput={(e) => setBirthField(e.currentTarget.value)}
								/>
								<span class="field-hint person-birth-hint">{BIRTH_FORMATS_HINT}</span>
							</label>

							<label class="person-field">
								<span class="person-field-label">Links — http(s) URLs</span>
								<MetaRows entries={urls} onChange={setUrls} label="URL" placeholder="https://…" maxLength={2048} disabled={busyState} />
							</label>

							<label class="person-field">
								<span class="person-field-label">Notes — markdown supported</span>
								<textarea
									id="personDescription"
									rows={7}
									maxLength={20000}
									placeholder="Free-form description…"
									disabled={busyState}
									value={description}
									onInput={(e) => setDescription(e.currentTarget.value)}
								/>
							</label>

							<footer class="modal-foot">
								<button type="submit" class="btn btn-accent" id="personMetaSave" disabled={busyState}>
									{busyState ? "Saving…" : "Save details"}
								</button>
								<button type="button" class="btn btn-ghost" id="personMetaCancel" onClick={cancelEdit} disabled={busyState}>
									Cancel
								</button>
							</footer>
						</form>
					)}

					{!editing && (
						<footer class="modal-foot">
							<button type="button" class="btn btn-ghost" data-close>Close</button>
						</footer>
					)}
				</div>
			}
		/>
	);
}
