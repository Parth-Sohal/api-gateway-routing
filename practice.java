import java.util.ArrayDeque;
import java.util.Arrays;

class practice {

    class SmartInteger {
        int num;
        int steps;

        SmartInteger(int num, int steps) {
            this.num = num;
            this.steps = steps;
        }
    }
    public static void main(String[] args) {
        int n = 8;

        int[] arr = new int[n];
        Arrays.fill(arr, -1);
        arr[1] = 0;
        ArrayDeque<SmartInteger> q = new ArrayDeque<>();
        q.push(new SmartInteger(n, 0));

        while (!q.isEmpty()) {
            int size = q.size();
            SmartInteger current = q.pop();
            int currentNum = current.num;
            int currentStep = current.steps;

            for(int i = 0; i < size; i++) {
                if (currentNum == 1) {
                    System.out.println(currentStep);
                    break;
                }

                if (arr[currentNum] != -1) {
                    
                }

                if (currentNum %2 == 0) {
                    q.push(new SmartInteger(currentNum/2, currentStep + 1));
                }
                else {
                    q.push(new SmartInteger(currentNum + 1, currentStep + 1));
                    q.push(new SmartInteger(currentNum - 1, currentStep + 1));
                }
            }
        }

    }
}

