import { Comet } from "../../Comet/comet"

function Index() {
	const handleClick = () => {
		Comet.call("task.insert", {
			title: "Test",
			description: "Test description",
		})
	}

	return (
		<>
			<h1>Index</h1>
			<p>
				Edit <code>test</code> and save to test HMR
			</p>
			<button onClick={handleClick}>Click me</button>
		</>
	)
}

export default Index

