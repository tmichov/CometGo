import { BrowserRouter, Routes as RRoutes, Route } from "react-router-dom";

const ROUTES = import.meta.glob('/src/pages/**/[a-z[]*.tsx', {eager: true}) as Record<string, any>;

let routes = Object.keys(ROUTES).map((route) => {
	let path = route
	.replace(/\/src\/pages|index|\.tsx$/g, '')
	.replace(/\[\.{3}.+\]/, '*')
	.replace(/\[(.+)\]/, ':$1')

	if (path !== '/' && path.endsWith('/')) {
		path = path.slice(0, -1)
	}

	return {
		path: `${path}`,
		element: ROUTES[route].default,
		children: [] as any
	}
});

const indices = [];
for (let i = routes.length-1; i >= 0; i--) {
	const path = routes[i].path;

	if (path.endsWith('/layout')) {
		const LayoutComponent = routes[i].element;

		indices.push(i);

		routes.map((route) => {
			if (route.path.startsWith(path.replace('/layout', '')) && route.path !== path) {
				const Comp = route.element;
				route.element = (
					<LayoutComponent>
						{ typeof Comp === 'object' ? Comp : <Comp /> }
					</LayoutComponent>
				)
			}
		});
	} 
}

indices.reverse().map((i) => {
	routes.splice(i, 1);
});

const customRoutes = routes.map((route, i) => { 
	const Comp = route.element;

	return (
		<Route key={`route-${i}`} path={route.path} element={typeof Comp == 'object' ? Comp : <Comp />} />
	)
});


export default function RoutesList() {
	return (
		<BrowserRouter>
			<RRoutes>
				{customRoutes}
				<Route path="*" element={<h1>404</h1>} />
			</RRoutes>
		</BrowserRouter>
	)
}
