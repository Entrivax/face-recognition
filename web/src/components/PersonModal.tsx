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
//
// All component-local state is signals. The one props mirror (props are not
// signals) exists because async continuations must see the person the modal
// currently shows — a rename patch swaps props.name on the live instance,
// and a computed over props would capture the first render's name.

import { useEffect, useRef } from "preact/hooks";
import { batch, useSignal, useSignalEffect, useComputed } from "@preact/signals";
import { Show } from "@preact/signals/utils";
import * as api from "../api";
import type { PersonDetail } from "../types";
import { plural } from "../util";
import { renderMarkdown } from "../markdown";
import {
	BIRTH_FORMATS_HINT,
	ageAt,
	birthAgeLine,
	formatBirthText,
	formatBirthTextual,
	parseBirthText,
	todayLocalISO,
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

	// Component-local state as signals: the loaded detail document, the
	// loading hint, the edit session and its form fields.
	const detail = useSignal<PersonDetail | null>(null);
	const loadHint = useSignal("Loading details…");
	const editing = useSignal(false);
	const busy = useSignal(false);

	// Edit-form state (kept flat so inputs stay simple and controlled).
	const aliases = useSignal<string[]>([]);
	const urls = useSignal<string[]>([]);
	// One dashed-template text field: YYYY-MM-DD / YYYY--DD / YYYY-MM /
	// YYYY / -MM-DD / -MM- (see birthdate.ts).
	const birthField = useSignal("");
	const description = useSignal("");

	const ageCalcOpen = useSignal(false);
	const ageCalcDate = useSignal("");
	const ageCalcResult = useComputed(() => {
		const b = detail.value?.birthdate;
		const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(ageCalcDate.value);
		if (!b || !m) return null;
		const at = new Date(`${ageCalcDate.value}T00:00:00Z`);
		const age = ageAt(b, at);
		if (age == null) return null;
		if (age < 0) return "not yet born";
		const exact = b.month != null && b.day != null;
		return `${exact ? "" : "≈ "}${plural(age, "year")} old on ${ageCalcDate.value}`;
	});

	// The person whose details are shown, mirrored from props (props are
	// not signals): loadDetail's stale guard and the photo target read it
	// at call time, so a rename patch cannot hand a detail document to the
	// wrong person.
	const personRef = useRef(person);
	personRef.current = person;

	const formRef = useRef<HTMLFormElement | null>(null);

	// Open: load the details (a rename patch re-runs this — [person] changes).
	useEffect(() => {
		batch(() => {
			detail.value = null;
			loadHint.value = "Loading details…";
			editing.value = false;
		});
		loadDetail(person);
	}, [person]);

	// Editing: focus the form's first input.
	useSignalEffect(() => {
		if (editing.value) formRef.current?.querySelector<HTMLElement>("input, textarea")?.focus();
	});

	// Drop responses for a person the user already left behind.
	async function loadDetail(name: string) {
		try {
			const j = await api.getPerson(name, app.repoId.value ?? "");
			if (personRef.current !== name) return;
			detail.value = j;
		} catch (e) {
			if (personRef.current === name) {
				loadHint.value = (e as Error).message || "Could not load the details.";
			}
		}
	}

	const close = () => {
		if (busy.value) return; // locked while a request is in flight
		onCloseRequest();
	};

	// Escape cancels the edit session first, then closes (call-time reads —
	// the handler cannot act on a stale render's state).
	const onEscape = () => {
		if (editing.value) cancelEdit();
		else close();
	};

	// The list row editors themselves are the shared MetaRows component.
	const setAliases = (next: string[]) => { aliases.value = next; };
	const setUrls = (next: string[]) => { urls.value = next; };

	function startEdit() {
		if (busy.value || editing.value || !detail.value) return;
		// The lists keep one trailing empty row as the always-ready slot;
		// blank rows (including that slot) are filtered out before saving.
		const d = detail.value;
		batch(() => {
			aliases.value = [...(d.aliases ?? []), ""];
			urls.value = [...(d.urls ?? []), ""];
			birthField.value = formatBirthText(d.birthdate);
			description.value = d.description ?? "";
			editing.value = true;
		});
	}

	function cancelEdit() {
		if (busy.value) return;
		editing.value = false;
	}

	useSignalEffect(() => {
		if (!editing.value) return;
		const onUnload = (e: BeforeUnloadEvent) => {
			e.preventDefault();
			e.returnValue = ""; // legacy Chrome/Safari; modern engines ignore custom text
		};
		window.addEventListener("beforeunload", onUnload);
		return () => window.removeEventListener("beforeunload", onUnload);
	});

	const saveEdit = async (e: Event) => {
		e.preventDefault();
		const name = personRef.current;
		if (busy.value || !name) return;
		// The birthdate text field is validated client-side (format +
		// component ranges); the server re-checks ranges and calendars.
		const birth = parseBirthText(birthField.value);
		if (!birth.ok) {
			toast.show(birth.error, "err");
			return;
		}
		const parts = birth.parts;
		const birthdate = parts.year == null && parts.month == null && parts.day == null
			? null
			: { year: parts.year ?? null, month: parts.month ?? null, day: parts.day ?? null };
		busy.value = true;
		try {
			const j = await api.setPersonMeta(name, app.repoId.value ?? "", {
				aliases: aliases.value.map((a) => a.trim()).filter(Boolean),
				birthdate,
				urls: urls.value.map((u) => u.trim()).filter(Boolean),
				description: description.value,
			});
			detail.value = j;
			editing.value = false;
			toast.show("Details saved.", "ok");
			app.refreshAll(); // the people list summary carries aliases
		} catch (err) {
			toast.show((err as Error).message || "Could not save the details.", "err");
		} finally {
			busy.value = false;
		}
	};

	function managePhotos() {
		if (busy.value || !personRef.current) return;
		// Stacked on top: this modal stays mounted (paused, state intact).
		modals.open("photos", { person: personRef.current });
	}

	// The avatar follows the live people list (alias/photo changes re-render
	// this modal with the new thumbnail URL) — a render-time read keyed on
	// the current person, not a computed: the closure would capture the
	// first render's props.name and go stale across a rename patch.
	const thumb = app.people.value.find((p) => p.name === person)?.thumb ?? null;

	// ---- writer-only header actions ----
	const writerActions = app.perms.value.writer && (
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
			locked={busy}
			backdropClickDisabled={editing.value}
			eyebrow="person"
			titleId="personTitle"
			title={detail.value?.name ?? person}
			headerActions={writerActions ?? undefined}
			onEscape={onEscape}
			onCloseRequest={close}
			body={
				<div class="modal-body person-detail">
					<Show when={detail} fallback={<p class="field-hint" id="personHint">{loadHint}</p>}>
						{(d) => {
							// ---- view helpers (from the loaded document) ----
							const birthText = formatBirthTextual(d.birthdate);
							// The same date in dashed numeric form (YYYY-MM-DD when complete).
							const birthIso = formatBirthText(d.birthdate);
							const birthExtra = birthAgeLine(d.birthdate);
							const hasMeta = !!((d.aliases?.length ?? 0) > 0 || birthText ||
								(d.urls?.length ?? 0) > 0 || (d.description ?? "") !== "");

							if (editing.value) {
								return (
									<form id="personMetaForm" class="person-edit" ref={formRef} onSubmit={saveEdit}>
										<label class="person-field">
											<span class="person-field-label">Aliases</span>
											<MetaRows entries={aliases} onChange={setAliases} label="Alias" placeholder="Alias" maxLength={120} disabled={busy.value} />
										</label>

										<label class="person-field">
											<span class="person-field-label">Birthdate — leave empty if unknown</span>
											<input
												id="personBirthText"
												type="text"
												placeholder="1965-03-02"
												autoComplete="off"
												spellcheck={false}
												disabled={busy}
												value={birthField}
												onInput={(e) => { birthField.value = e.currentTarget.value; }}
											/>
											<span class="field-hint person-birth-hint">{BIRTH_FORMATS_HINT}</span>
										</label>

										<label class="person-field">
											<span class="person-field-label">Links — http(s) URLs</span>
											<MetaRows entries={urls} onChange={setUrls} label="URL" placeholder="https://…" maxLength={2048} disabled={busy.value} />
										</label>

										<label class="person-field">
											<span class="person-field-label">Notes — markdown supported</span>
											<textarea
												id="personDescription"
												rows={7}
												maxLength={20000}
												placeholder="Free-form description…"
												disabled={busy}
												value={description}
												onInput={(e) => { description.value = e.currentTarget.value; }}
											/>
										</label>

										<footer class="modal-foot">
											<button type="submit" class="btn btn-accent" id="personMetaSave" disabled={busy}>
												{busy.value ? "Saving…" : "Save details"}
											</button>
											<button type="button" class="btn btn-ghost" id="personMetaCancel" onClick={cancelEdit} disabled={busy}>
												Cancel
											</button>
										</footer>
									</form>
								);
							}

							return (
								<div id="personDetail">
									<div class="person-detail-head">
										<Avatar src={thumb ?? ""} name={d.name} />
										<p class="field-hint">{plural(d.photos)} enrolled</p>
									</div>

									{!hasMeta && (
										<p class="field-hint person-empty" id="personEmptyHint">
											No details recorded for this person.
											{app.perms.value.writer ? " Use “Edit details” to add some." : ""}
										</p>
									)}

									{hasMeta && (
										<div class="person-meta-list">
											{d.aliases && d.aliases.length > 0 && (
												<section class="person-section">
													<h3 class="person-section-label">Aliases</h3>
													<div class="person-aliases">
														{/* wholesale-replaced document — For tracks by identity,
														    so the plain spans stay a .map */}
														{d.aliases.map((a) => <span class="person-alias" key={a}>{a}</span>)}
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
													{birthExtra && (d.birthdate?.year != null ? (
											<button
												type="button"
												class="person-birth-extra person-birth-toggle"
												id="personAgeToggle"
												aria-expanded={ageCalcOpen.value}
												title="Age calculator"
												onClick={() => {
													if (!ageCalcOpen.value && !ageCalcDate.value) ageCalcDate.value = todayLocalISO();
													ageCalcOpen.value = !ageCalcOpen.value;
												}}
											>
												{birthExtra}
											</button>
										) : (
											// A countdown-only line (no year) — no age is computable.
											<p class="person-birth-extra">{birthExtra}</p>
										))}
										<Show when={ageCalcOpen}>
											{() => (
												<div class="person-age-calc" id="personAgeCalc">
													<label class="person-age-calc-label" for="personAgeCalcDate">Age on</label>
													<input
														id="personAgeCalcDate"
														type="date"
														value={ageCalcDate}
														onInput={(e) => { ageCalcDate.value = e.currentTarget.value; }}
													/>
													<Show when={ageCalcResult}>
														{(r) => <p class="person-age-calc-result">{r}</p>}
													</Show>
												</div>
											)}
										</Show>
												</section>
											)}

											{d.urls && d.urls.length > 0 && (
												<section class="person-section">
													<h3 class="person-section-label">Links</h3>
													<ul class="person-links">
														{d.urls.map((u) => (
															<li key={u}>
																<a href={u} target="_blank" rel="noopener noreferrer">{u}</a>
															</li>
														))}
													</ul>
												</section>
											)}

											{(d.description ?? "") !== "" && (
												<section class="person-section">
													<h3 class="person-section-label">Notes</h3>
													{/* markdown.ts sanitizes before this ever hits the DOM */}
													<div
														class="person-prose"
														dangerouslySetInnerHTML={{ __html: renderMarkdown(d.description ?? "") }}
													/>
												</section>
											)}
										</div>
									)}
								</div>
							);
						}}
					</Show>

					<Show when={() => !editing.value} fallback={null}>
						{() => (
							<footer class="modal-foot">
								<button type="button" class="btn btn-ghost" data-close>Close</button>
							</footer>
						)}
					</Show>
				</div>
			}
		/>
	);
}
