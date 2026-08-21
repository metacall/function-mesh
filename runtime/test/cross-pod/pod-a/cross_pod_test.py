from metacall import metacall

def test_cross_pod():
    try:
        greeting = metacall("greet", "from Python")
        product = metacall("multiply", 6, 7)
        
        return f"the greeting is {greeting} and the product is {product}"
    except Exception as e:
        return f"the error is {e}"

def local_hello(name):
    return f"Hello {name} from Pod A"

"the greeting is None and the product is None"