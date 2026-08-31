/* recogn — the enrolled-people list. */

import { el } from "./dom.js";
import { showToast, escapeHtml, initials } from "./util.js";
import { getPeople, deletePerson } from "./api.js";
import { state } from "./state.js";
import { openPhotosModal } from "./photos.js";

// Called after a person is removed so main.js can refresh health, etc.
let onChange = () => {};
export function onPeopleChange(fn) { onChange = fn; }

export async function loadPeople() {
	try {
		const j = await getPeople();
		const people = j.people || [];
		state.peopleNames = people.map((p) => p.name);
		el.peopleCount.textContent = people.length
			? `${people.length} ${people.length === 1 ? "person" : "people"} in the database`
			: "No one enrolled yet.";
		el.peopleList.innerHTML = "";
		people.forEach((p) => {
			const li = document.createElement("li");
			li.className = "person-row";
			li.dataset.name = p.name; // read by the search filter
			const avatar = p.thumb
				? `<img class="person-avatar person-avatar-img" src="${escapeHtml(p.thumb)}" alt="" data-initials="${escapeHtml(initials(p.name))}">`
				: `<span class="person-avatar">${escapeHtml(initials(p.name))}</span>`;
			li.innerHTML = `
				<button type="button" class="person-avatar-btn" title="Manage photos"
								aria-label="Manage photos for ${escapeHtml(p.name)}">${avatar}</button>
				<button type="button" class="person-open" title="Manage photos"
								aria-label="Manage photos for ${escapeHtml(p.name)}">
					<span class="person-name">${escapeHtml(p.name)}</span>
					<span class="person-count">${p.photos} photo(s)</span>
				</button>
				<button class="person-del" title="Remove ${escapeHtml(p.name)}" aria-label="Remove ${escapeHtml(p.name)}">×</button>`;
			const img = li.querySelector("img.person-avatar-img");
			if (img) {
				// Missing/broken thumbnail (e.g. deleted sidecar) → initials.
				img.addEventListener("error", () => {
					const span = document.createElement("span");
					span.className = "person-avatar";
					span.textContent = img.dataset.initials || "";
					img.replaceWith(span);
				});
			}
			li.querySelector(".person-avatar-btn")
				.addEventListener("click", () => openPhotosModal(p.name));
			li.querySelector(".person-open")
				.addEventListener("click", () => openPhotosModal(p.name));
			li.querySelector(".person-del").addEventListener("click", () => removePerson(p.name));
			el.peopleList.appendChild(li);
		});
		applyFilter();
	} catch (e) {
		el.peopleCount.textContent = "Could not load people.";
	}
}

// ---- search filter ----
// Case-insensitive substring match over the rendered rows; the current filter
// stays active across reloads. An all-filtered-out list shows a hint row.

function applyFilter() {
	const q = el.peopleSearch.value.trim().toLowerCase();
	let visible = 0;
	for (const li of el.peopleList.children) {
		if (!(li instanceof HTMLLIElement) || !li.dataset.name) continue;
		const hit = q === "" || li.dataset.name.toLowerCase().includes(q);
		li.hidden = !hit;
		if (hit) visible++;
	}
	// filter hint
	let hint = el.peopleList.querySelector(".people-nomatch");
	if (visible === 0 && q !== "" && el.peopleList.children.length > 0) {
		if (!hint) {
			hint = document.createElement("li");
			hint.className = "people-nomatch";
			el.peopleList.appendChild(hint);
		}
		hint.textContent = `No people match “${el.peopleSearch.value.trim()}”.`;
		hint.hidden = false;
	} else if (hint) {
		hint.hidden = true;
	}
}

el.peopleSearch.addEventListener("input", applyFilter);

async function removePerson(name) {
	if (!confirm(`Remove ${name} and all their photos from the database?`)) return;
	try {
		await deletePerson(name);
		showToast(`Removed ${name}.`, "ok");
		loadPeople();
		onChange();
	} catch (e) {
		showToast(e.message, "err");
	}
}
