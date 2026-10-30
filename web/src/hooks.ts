import { signal, useComputed } from "@preact/signals"
import { useEffect } from "preact/hooks"

const locationSignal = signal({
	pathname: window.location.pathname,
	search: window.location.search,
	hash: window.location.hash,
})

export function useLocationUpdater() {
	useEffect(() => {
		const updateLocation = () => {
			locationSignal.value = {
				pathname: window.location.pathname,
				search: window.location.search,
				hash: window.location.hash,
			}
		}

		const abortController = new AbortController()
		const signal = abortController.signal

		window.addEventListener('popstate', updateLocation, { signal })
		window.addEventListener('hashchange', updateLocation, { signal });

		return () => abortController.abort()
	}, [])
}

export function usePathname() {
	return useComputed(() => locationSignal.value.pathname)
}