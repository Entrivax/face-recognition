/* recogn — cross-module shared state.
	 Kept tiny on purpose: anything bigger belongs in function arguments. */

export const state = {
	peopleNames: [], // refreshed by people.js on every load; read by enroll.js
};
