from metacall import metacall

def test_cross_pod():
    try:
        greeting = metacall("greet", "from Python")
        product = metacall("multiply", 6, 7)
        
        return {
            "greeting": greeting,      # "Hi from Python from Node!"
            "product": product         # 42 
        }
    except Exception as e:
        return {"error": str(e)}

def local_hello(name):
    return f"Hello {name} from Pod A"
