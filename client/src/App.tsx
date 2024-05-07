import './App.css'
import RoutesList from './routes'
import { Comet } from './Comet/comet'

Comet.init()

function App() {
	return (
		<>
			<RoutesList />
		</>
	)
}

export default App
